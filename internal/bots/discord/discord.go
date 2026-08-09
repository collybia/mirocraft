// Package discord runs the panel's Discord bot.
//
// Everything it decides is in botcore; this package is the translation between
// Discord's interaction model and that.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/collybia/mirocraft/internal/bots/botcore"
	"github.com/collybia/mirocraft/internal/panelclient"
	"github.com/collybia/mirocraft/internal/store"
)

// messageLimit is Discord's cap on a message. Output is truncated to fit
// rather than rejected: half a console is worth more than an error saying it
// was too long.
const messageLimit = 1900

// commandTimeout bounds one command. Discord itself gives three seconds to
// acknowledge an interaction, which is why every command defers its reply
// first and answers afterwards.
const commandTimeout = 30 * time.Second

// Bot is a running Discord session.
type Bot struct {
	session  *discordgo.Session
	commands *botcore.Commands
	log      *slog.Logger

	mu      sync.Mutex
	account string
}

// New builds a bot. It does not connect; call Start.
func New(token string, client *panelclient.Client, panelURL string, log *slog.Logger) (*Bot, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("discord: the bot token is empty")
	}
	if log == nil {
		log = slog.Default()
	}

	session, err := discordgo.New("Bot " + token)
	if err != nil {
		// The token is not in the message: this error is logged and shown in
		// the panel, and a credential belongs in neither.
		return nil, fmt.Errorf("discord: building the session: %w", err)
	}
	// Slash commands need no privileged intents, and asking for ones that go
	// unused is what makes a bot fail verification later.
	session.Identify.Intents = discordgo.IntentsNone

	bot := &Bot{
		session: session,
		log:     log,
		commands: &botcore.Commands{
			Client:   client,
			Provider: store.ProviderDiscord,
			PanelURL: panelURL,
		},
	}
	session.AddHandler(bot.onInteraction)
	return bot, nil
}

// Start connects and registers the slash commands.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord: connecting: %w", err)
	}

	user := b.session.State.User
	if user == nil {
		_ = b.session.Close()
		return errors.New("discord: connected but the session has no account")
	}

	b.mu.Lock()
	b.account = user.Username
	b.mu.Unlock()

	// Registered globally rather than per guild: a panel's bot is usually in
	// one server, and a guild-scoped registration would silently stop working
	// the moment someone invited it to a second.
	if _, err := b.session.ApplicationCommandBulkOverwrite(
		user.ID, "", definitions(), discordgo.WithContext(ctx)); err != nil {
		_ = b.session.Close()
		return fmt.Errorf("discord: registering commands: %w", err)
	}

	b.log.Info("discord bot connected", slog.String("account", user.Username))
	return nil
}

// Account returns the bot's name on Discord, empty until it has connected.
func (b *Bot) Account() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.account
}

// Stop closes the session. The registered commands are left in place: they are
// harmless while the bot is off — Discord answers that the application did not
// respond — and re-registering on every start would spend the rate limit.
func (b *Bot) Stop() error { return b.session.Close() }

// definitions describes the slash commands.
func definitions() []*discordgo.ApplicationCommand {
	server := &discordgo.ApplicationCommandOption{
		Type:        discordgo.ApplicationCommandOptionString,
		Name:        "server",
		Description: "имя сервера",
		Required:    true,
	}

	return []*discordgo.ApplicationCommand{
		{Name: "servers", Description: "Ваши серверы и их состояние"},
		{
			Name:        "status",
			Description: "Подробности одного сервера",
			Options:     []*discordgo.ApplicationCommandOption{server},
		},
		{
			Name:        "start",
			Description: "Запустить сервер",
			Options:     []*discordgo.ApplicationCommandOption{server},
		},
		{
			Name:        "stop",
			Description: "Остановить сервер",
			Options:     []*discordgo.ApplicationCommandOption{server},
		},
		{
			Name:        "restart",
			Description: "Перезапустить сервер",
			Options:     []*discordgo.ApplicationCommandOption{server},
		},
		{
			Name:        "cmd",
			Description: "Выполнить команду на сервере",
			Options: []*discordgo.ApplicationCommandOption{
				server,
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "command",
					Description: "команда без ведущего слэша",
					Required:    true,
				},
			},
		},
		{
			Name:        "console",
			Description: "Последние строки консоли",
			Options: []*discordgo.ApplicationCommandOption{
				server,
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "lines",
					Description: "сколько строк",
					Required:    false,
					MinValue:    ptr(1.0),
					MaxValue:    botcore.MaxConsoleLines,
				},
			},
		},
		{
			Name:        "link",
			Description: "Привязать этот Discord к учётной записи панели",
			Options: []*discordgo.ApplicationCommandOption{{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "code",
				Description: "код из панели",
				Required:    true,
			}},
		},
		{Name: "unlink", Description: "Отвязать этот Discord от панели"},
	}
}

func ptr[T any](v T) *T { return &v }

// onInteraction dispatches one slash command.
func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	// A panic in a handler must not take the daemon down with it: this bot
	// runs in the same process as every Minecraft server the panel manages.
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("discord handler panicked", slog.Any("panic", r))
		}
	}()

	user := interactionUser(i)
	if user == nil {
		b.log.Warn("discord interaction with no user")
		return
	}

	// Acknowledged first: Discord discards an interaction that is not answered
	// within three seconds, and a panel call can take longer than that.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		b.log.Warn("acknowledging a discord interaction failed", slog.String("error", err.Error()))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	reply := b.dispatch(ctx, user.ID, i.ApplicationCommandData())

	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: ptr(render(reply)),
	}); err != nil {
		b.log.Warn("answering a discord interaction failed", slog.String("error", err.Error()))
	}
}

// options is one command's arguments, by name.
type options map[string]*discordgo.ApplicationCommandInteractionDataOption

// handler answers one command.
type handler func(ctx context.Context, userID string, opts options) botcore.Reply

// handlers maps a command name to what answers it.
//
// A table rather than a switch so that the registration and the dispatch can
// be compared: a command offered in Discord's menu and missing here answers
// "no such command", which reads as a broken bot. The test walks definitions()
// against these keys, and that comparison is only worth anything because both
// sides are the real lists.
func (b *Bot) handlers() map[string]handler {
	return map[string]handler{
		"servers": func(ctx context.Context, userID string, _ options) botcore.Reply {
			return b.commands.Servers(ctx, userID)
		},
		"status": func(ctx context.Context, userID string, opts options) botcore.Reply {
			return b.commands.Status(ctx, userID, opts.str("server"))
		},
		"start": func(ctx context.Context, userID string, opts options) botcore.Reply {
			return b.commands.Power(ctx, userID, opts.str("server"), panelclient.ActionStart)
		},
		"stop": func(ctx context.Context, userID string, opts options) botcore.Reply {
			return b.commands.Power(ctx, userID, opts.str("server"), panelclient.ActionStop)
		},
		"restart": func(ctx context.Context, userID string, opts options) botcore.Reply {
			return b.commands.Power(ctx, userID, opts.str("server"), panelclient.ActionRestart)
		},
		"cmd": func(ctx context.Context, userID string, opts options) botcore.Reply {
			return b.commands.Command(ctx, userID, opts.str("server"), opts.str("command"))
		},
		"console": func(ctx context.Context, userID string, opts options) botcore.Reply {
			return b.commands.Console(ctx, userID, opts.str("server"), opts.num("lines"))
		},
		"link": func(ctx context.Context, userID string, opts options) botcore.Reply {
			return b.commands.Link(ctx, userID, opts.str("code"))
		},
		"unlink": func(ctx context.Context, userID string, _ options) botcore.Reply {
			return b.commands.Unlink(ctx, userID)
		},
	}
}

// dispatch turns one command into an answer.
func (b *Bot) dispatch(ctx context.Context, userID string, data discordgo.ApplicationCommandInteractionData) botcore.Reply {
	answer, ok := b.handlers()[data.Name]
	if !ok {
		return botcore.Reply{Text: "Такой команды нет.", Ephemeral: true}
	}
	return answer(ctx, userID, optionMap(data.Options))
}

// interactionUser finds who invoked a command, which lives in different places
// in a guild and in a direct message.
func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

func optionMap(given []*discordgo.ApplicationCommandInteractionDataOption) options {
	out := make(options, len(given))
	for _, option := range given {
		out[option.Name] = option
	}
	return out
}

// str returns a string argument, empty when it was not given.
func (o options) str(name string) string {
	if option, ok := o[name]; ok {
		return option.StringValue()
	}
	return ""
}

// num returns a numeric argument, zero when it was not given.
func (o options) num(name string) int {
	if option, ok := o[name]; ok {
		return int(option.IntValue())
	}
	return 0
}

// render turns a reply into a Discord message.
//
// Exported for the tests, which check the formatting without a live session:
// the truncation and the code fence are the parts that break, and they break
// on exactly the messages nobody sends while developing.
func render(reply botcore.Reply) string {
	text := reply.Text
	if reply.Monospace {
		// The fence itself costs characters, so the budget accounts for it.
		const fence = "```\n"
		if len(text) > messageLimit-2*len(fence) {
			text = truncate(text, messageLimit-2*len(fence))
		}
		return fence + text + "\n```"
	}
	return truncate(text, messageLimit)
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

	// A cut in the middle of a multi-byte character produces a replacement
	// glyph, and a cut in the middle of a line produces something that looks
	// like a log entry and is not one.
	trimmed := strings.ToValidUTF8(text[:cut], "")
	if at := strings.LastIndexByte(trimmed, '\n'); at > 0 {
		trimmed = trimmed[:at]
	}
	return trimmed + notice
}
