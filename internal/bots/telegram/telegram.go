// Package telegram runs the panel's Telegram bot.
//
// Like the Discord package, this is only the translation between one chat
// platform and botcore; everything it decides lives there.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/collybia/mirocraft/internal/bots/botcore"
	"github.com/collybia/mirocraft/internal/panelclient"
	"github.com/collybia/mirocraft/internal/store"
)

// messageLimit is Telegram's cap on a message, minus room for the code fence.
const messageLimit = 3800

// pollTimeout is how long each getUpdates call waits. Long polling rather than
// a webhook: a webhook needs a public address with a valid certificate, and a
// panel behind NAT — which is most of them — has neither.
const pollTimeout = 30

// commandTimeout bounds one command.
const commandTimeout = 30 * time.Second

// Bot is a running Telegram session.
type Bot struct {
	api      *tgbotapi.BotAPI
	token    string
	commands *botcore.Commands
	log      *slog.Logger

	mu      sync.Mutex
	account string
	cancel  context.CancelFunc
	done    chan struct{}
}

// New builds a bot. It does not connect; call Start.
func New(token string, client *panelclient.Client, panelURL string, log *slog.Logger) (*Bot, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("telegram: the bot token is empty")
	}
	if log == nil {
		log = slog.Default()
	}

	return &Bot{
		log: log,
		commands: &botcore.Commands{
			Client:   client,
			Provider: store.ProviderTelegram,
			PanelURL: panelURL,
		},
		token: token,
	}, nil
}

// Start connects and begins polling.
func (b *Bot) Start(ctx context.Context) error {
	// NewBotAPI verifies the token against Telegram, so this is the connection
	// step rather than construction.
	api, err := tgbotapi.NewBotAPI(b.token)
	if err != nil {
		// Wrapped without the token: this error is logged and shown in the
		// panel, and a credential belongs in neither.
		return fmt.Errorf("telegram: connecting: %w", err)
	}
	b.api = api

	b.mu.Lock()
	b.account = api.Self.UserName
	b.mu.Unlock()

	// The command list is what fills Telegram's menu. A failure here is worth
	// a line but not the connection: the commands still work typed out.
	if _, err := api.Request(tgbotapi.NewSetMyCommands(definitions()...)); err != nil {
		b.log.Warn("registering telegram commands failed", slog.String("error", err.Error()))
	}

	pollCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	b.cancel = cancel
	b.done = make(chan struct{})

	config := tgbotapi.NewUpdate(0)
	config.Timeout = pollTimeout
	updates := api.GetUpdatesChan(config)

	go b.poll(pollCtx, updates)

	b.log.Info("telegram bot connected", slog.String("account", api.Self.UserName))
	return nil
}

// Account returns the bot's @name, empty until it has connected.
func (b *Bot) Account() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.account
}

// Stop ends the polling loop and waits for it to finish.
func (b *Bot) Stop() error {
	if b.cancel == nil {
		return nil
	}
	b.api.StopReceivingUpdates()
	b.cancel()

	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		// The library's channel can outlive one poll interval; not worth
		// holding a shutdown for.
		b.log.Warn("telegram poll loop did not stop in time")
	}
	return nil
}

// poll reads updates until the context ends.
func (b *Bot) poll(ctx context.Context, updates tgbotapi.UpdatesChannel) {
	defer close(b.done)

	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			b.handle(ctx, update)
		}
	}
}

// handle answers one update.
func (b *Bot) handle(ctx context.Context, update tgbotapi.Update) {
	// A panic here must not take the daemon down: it runs in the same process
	// as every Minecraft server the panel manages.
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("telegram handler panicked", slog.Any("panic", r))
		}
	}()

	if update.Message == nil || !update.Message.IsCommand() || update.Message.From == nil {
		return
	}

	// The user's id, not the chat's: a group chat has one id and many people,
	// and acting for the chat would let anyone in it use anyone's servers.
	userID := strconv.FormatInt(update.Message.From.ID, 10)

	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	reply := b.dispatch(callCtx, userID, update.Message.Command(), update.Message.CommandArguments())

	message := tgbotapi.NewMessage(update.Message.Chat.ID, render(reply))
	message.ReplyToMessageID = update.Message.MessageID
	if reply.Monospace {
		message.ParseMode = tgbotapi.ModeMarkdownV2
	}
	if _, err := b.api.Send(message); err != nil {
		b.log.Warn("answering a telegram command failed", slog.String("error", err.Error()))
	}
}

// dispatch turns one command into an answer.
//
// Telegram passes arguments as one string, so the first word is the server
// name and the rest, where a command takes one, is the argument.
func (b *Bot) dispatch(ctx context.Context, userID, command, arguments string) botcore.Reply {
	name, rest := splitFirst(arguments)

	switch command {
	case "servers":
		return b.commands.Servers(ctx, userID)
	case "status":
		return b.commands.Status(ctx, userID, name)
	case "start":
		// Telegram sends /start by itself when someone opens the bot for the
		// first time, so a bare /start is a greeting rather than a request to
		// start a server that was not named.
		if name == "" {
			return b.greeting()
		}
		return b.commands.Power(ctx, userID, name, panelclient.ActionStart)
	case "help":
		return b.greeting()
	case "stop":
		return b.commands.Power(ctx, userID, name, panelclient.ActionStop)
	case "restart":
		return b.commands.Power(ctx, userID, name, panelclient.ActionRestart)
	case "cmd":
		return b.commands.Command(ctx, userID, name, rest)
	case "console":
		lines, _ := strconv.Atoi(strings.TrimSpace(rest))
		return b.commands.Console(ctx, userID, name, lines)
	case "link":
		return b.commands.Link(ctx, userID, strings.TrimSpace(arguments))
	case "unlink":
		return b.commands.Unlink(ctx, userID)
	default:
		return botcore.Reply{Text: "Такой команды нет. /servers покажет ваши серверы.", Ephemeral: true}
	}
}

// greeting is what a bare /start answers with.
func (b *Bot) greeting() botcore.Reply {
	return botcore.Reply{Text: strings.Join([]string{
		"Это бот панели Mirocraft.",
		"",
		"Сначала привяжите аккаунт: возьмите код в панели и пришлите /link <код>.",
		"Дальше:",
		"  /servers — ваши серверы",
		"  /status имя — подробности",
		"  /start имя, /stop имя, /restart имя",
		"  /cmd имя команда — выполнить команду",
		"  /console имя — последние строки",
	}, "\n")}
}

// splitFirst separates the first word from the rest.
func splitFirst(text string) (first, rest string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", ""
	}
	if at := strings.IndexAny(trimmed, " \t"); at >= 0 {
		return trimmed[:at], strings.TrimSpace(trimmed[at+1:])
	}
	return trimmed, ""
}

// definitions is the command list Telegram shows in its menu.
func definitions() []tgbotapi.BotCommand {
	return []tgbotapi.BotCommand{
		{Command: "servers", Description: "ваши серверы и их состояние"},
		{Command: "status", Description: "подробности сервера: /status имя"},
		{Command: "start", Description: "запустить: /start имя"},
		{Command: "stop", Description: "остановить: /stop имя"},
		{Command: "restart", Description: "перезапустить: /restart имя"},
		{Command: "cmd", Description: "выполнить команду: /cmd имя команда"},
		{Command: "console", Description: "последние строки: /console имя [сколько]"},
		{Command: "link", Description: "привязать аккаунт: /link код"},
		{Command: "unlink", Description: "отвязать аккаунт"},
	}
}

// render turns a reply into a Telegram message.
func render(reply botcore.Reply) string {
	text := reply.Text
	if reply.Monospace {
		const fence = "```"
		if len(text) > messageLimit {
			text = truncate(text, messageLimit)
		}
		// Escaped, because MarkdownV2 treats a backslash and a backtick inside
		// a fence as markup and a Minecraft log contains both.
		return fence + "\n" + escapeFenced(text) + "\n" + fence
	}
	return truncate(text, messageLimit)
}

// escapeFenced escapes what MarkdownV2 still reads inside a code fence.
func escapeFenced(text string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "`", "\\`")
	return replacer.Replace(text)
}

// truncate cuts on a line boundary where it can, and says that it cut.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const notice = "\n… обрезано"
	cut := limit - len(notice)
	if cut < 0 {
		cut = 0
	}

	trimmed := strings.ToValidUTF8(text[:cut], "")
	if at := strings.LastIndexByte(trimmed, '\n'); at > 0 {
		trimmed = trimmed[:at]
	}
	return trimmed + notice
}
