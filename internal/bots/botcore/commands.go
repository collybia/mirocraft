// Package botcore holds everything the chat bots do that is not about a
// particular chat platform.
//
// Both bots answer the same questions with the same words; only the way a
// message is put on the wire differs. Writing that twice would guarantee the
// two drift, and the second one would drift silently — nobody reads the
// Telegram bot's wording while working on Discord.
package botcore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/collybia/mirocraft/internal/panelclient"
)

// MaxConsoleLines is what /console returns by default. A chat message has a
// size limit on both platforms, and a hundred lines of a Minecraft log is
// already past what anyone reads in a scrollback.
const MaxConsoleLines = 20

// Commands answers chat commands against a panel.
type Commands struct {
	// Client is the bot's own client, acting as itself. Every command derives
	// a delegated client from it.
	Client *panelclient.Client
	// Provider is "discord" or "telegram".
	Provider string
	// PanelURL is shown to people who need to open the panel, for instance to
	// get a linking code.
	PanelURL string
}

// Reply is an answer to render. Text is plain; a platform that wants bold or
// a code block wraps it.
type Reply struct {
	Text string
	// Monospace asks the platform to render the text in a code block, for
	// console output and tables where alignment carries meaning.
	Monospace bool
	// Ephemeral asks for an answer only the person who asked can see. Set for
	// errors and for anything naming a server someone else need not know
	// about.
	Ephemeral bool
}

// plain, mono and problem build the three shapes of answer.
func plain(format string, args ...any) Reply {
	return Reply{Text: fmt.Sprintf(format, args...)}
}

func mono(text string) Reply {
	return Reply{Text: text, Monospace: true, Ephemeral: true}
}

func problem(format string, args ...any) Reply {
	return Reply{Text: fmt.Sprintf(format, args...), Ephemeral: true}
}

// forUser returns a client acting for one chat account.
func (c *Commands) forUser(externalID string) *panelclient.Client {
	return c.Client.For(c.Provider, externalID)
}

// Servers answers /servers.
func (c *Commands) Servers(ctx context.Context, externalID string) Reply {
	servers, err := c.forUser(externalID).ListServers(ctx, panelclient.ListServersOptions{})
	if err != nil {
		return c.explain(err, "")
	}
	if len(servers) == 0 {
		return plain("У вас нет серверов. Создать можно в панели: %s", c.PanelURL)
	}

	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })

	var b strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&b, "%s  %s  %s %s", statusMark(s.Status), s.Name, s.Core, s.Version)
		if s.Metrics != nil && s.Metrics.PlayersOnline != nil {
			fmt.Fprintf(&b, "  игроков: %d", *s.Metrics.PlayersOnline)
		}
		b.WriteByte('\n')
	}
	return mono(strings.TrimRight(b.String(), "\n"))
}

// Status answers /status.
func (c *Commands) Status(ctx context.Context, externalID, name string) Reply {
	client := c.forUser(externalID)

	server, err := client.FindServer(ctx, name)
	if err != nil {
		return c.explain(err, name)
	}
	// Found by name, then fetched by id: the list does not carry live metrics,
	// and a status without them answers half the question.
	server, err = client.GetServer(ctx, server.ID)
	if err != nil {
		return c.explain(err, name)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", statusMark(server.Status), server.Name)
	fmt.Fprintf(&b, "ядро:    %s %s\n", server.Core, server.Version)
	fmt.Fprintf(&b, "статус:  %s\n", statusWord(server.Status))
	fmt.Fprintf(&b, "порт:    %d\n", server.Port)
	fmt.Fprintf(&b, "память:  %d МБ", server.RAMMb)

	if m := server.Metrics; m != nil {
		fmt.Fprintf(&b, "\nзанято:  %d МБ, CPU %.0f%%", m.RAMUsedMb, m.CPUPercent)
		fmt.Fprintf(&b, "\nаптайм:  %s", humanDuration(time.Duration(m.UptimeSeconds)*time.Second))
		if m.PlayersOnline != nil && m.PlayersMax != nil {
			fmt.Fprintf(&b, "\nигроки:  %d из %d", *m.PlayersOnline, *m.PlayersMax)
		}
	}
	return mono(b.String())
}

// Power answers /start, /stop and /restart.
//
// It returns as soon as the panel accepts the request rather than waiting for
// the server to come up: starting one takes minutes, and a chat command that
// sits silent for minutes reads as broken.
func (c *Commands) Power(ctx context.Context, externalID, name, action string) Reply {
	client := c.forUser(externalID)

	server, err := client.FindServer(ctx, name)
	if err != nil {
		return c.explain(err, name)
	}

	if _, err := client.Power(ctx, server.ID, action, 0); err != nil {
		return c.explain(err, server.Name)
	}

	switch action {
	case panelclient.ActionStart:
		return plain("Запускаю %s. Это занимает до минуты — посмотрите /status.", server.Name)
	case panelclient.ActionStop:
		return plain("Останавливаю %s, мир сохраняется.", server.Name)
	case panelclient.ActionRestart:
		return plain("Перезапускаю %s.", server.Name)
	case panelclient.ActionKill:
		return plain("Гашу %s без сохранения.", server.Name)
	default:
		return plain("Готово.")
	}
}

// Command answers /cmd.
func (c *Commands) Command(ctx context.Context, externalID, name, command string) Reply {
	if strings.TrimSpace(command) == "" {
		return problem("Нечего выполнять: команда пустая.")
	}

	client := c.forUser(externalID)
	server, err := client.FindServer(ctx, name)
	if err != nil {
		return c.explain(err, name)
	}

	if err := client.SendCommand(ctx, server.ID, command); err != nil {
		return c.explain(err, server.Name)
	}
	// The output arrives in the console, not in the response: the server
	// answers a command whenever it feels like it, and often not at all.
	return plain("Отправил на %s: %s\nОтвет смотрите в /console.", server.Name, command)
}

// Console answers /console.
func (c *Commands) Console(ctx context.Context, externalID, name string, lines int) Reply {
	if lines <= 0 || lines > MaxConsoleLines {
		lines = MaxConsoleLines
	}

	client := c.forUser(externalID)
	server, err := client.FindServer(ctx, name)
	if err != nil {
		return c.explain(err, name)
	}

	history, err := client.ConsoleHistory(ctx, server.ID, lines)
	if err != nil {
		return c.explain(err, server.Name)
	}
	if len(history) == 0 {
		return problem("Консоль %s пуста — сервер, похоже, не запускался.", server.Name)
	}

	var b strings.Builder
	for _, line := range history {
		b.WriteString(line.Text)
		b.WriteByte('\n')
	}
	return mono(strings.TrimRight(b.String(), "\n"))
}

// Link answers /link.
func (c *Commands) Link(ctx context.Context, externalID, code string) Reply {
	if strings.TrimSpace(code) == "" {
		return problem("Нужен код из панели: %s", c.PanelURL)
	}

	// As itself, not on behalf of anyone: there is nobody to act for until
	// this succeeds.
	linked, err := c.Client.Link(ctx, c.Provider, code, externalID)
	switch {
	case errors.Is(err, panelclient.ErrLinkCodeInvalid):
		return problem("Код не подошёл — он живёт десять минут и срабатывает один раз. Возьмите новый в панели: %s", c.PanelURL)
	case errors.Is(err, panelclient.ErrLinkTaken):
		return problem("Этот аккаунт уже привязан к другой учётной записи панели. Отвяжите его через /unlink или войдите в ту учётную запись.")
	case err != nil:
		return c.explain(err, "")
	}

	return problem("Готово, вы — %s. Теперь /servers покажет ваши серверы.", linked.Email)
}

// Unlink answers /unlink.
func (c *Commands) Unlink(ctx context.Context, externalID string) Reply {
	err := c.forUser(externalID).Unlink(ctx, c.Provider)
	switch {
	case errors.Is(err, panelclient.ErrNotFound), errors.Is(err, panelclient.ErrForbidden):
		return problem("Ваш аккаунт и так не привязан.")
	case err != nil:
		return c.explain(err, "")
	}
	return problem("Отвязал. Команды больше не будут работать, пока вы не привяжетесь снова.")
}

// explain turns an error into something worth reading in a chat.
//
// The panel's own message is used where it is specific, and replaced where it
// is not: "not allowed" is true and useless, and the reason a bot user is not
// allowed is almost always that they have not linked their account.
func (c *Commands) explain(err error, name string) Reply {
	switch {
	case errors.Is(err, panelclient.ErrForbidden), errors.Is(err, panelclient.ErrUnauthorized):
		return problem("Ваш чат-аккаунт не привязан к панели. Возьмите код в %s и пришлите /link <код>.", c.PanelURL)
	case errors.Is(err, panelclient.ErrNotFound):
		if name != "" {
			return problem("Сервера %q у вас нет. /servers покажет, какие есть.", name)
		}
		return problem("Не нашёл.")
	case errors.Is(err, panelclient.ErrAlreadyRunning):
		return problem("%s уже запущен.", displayName(name))
	case errors.Is(err, panelclient.ErrNotRunning):
		return problem("%s не запущен.", displayName(name))
	case errors.Is(err, panelclient.ErrRateLimited):
		var apiErr *panelclient.Error
		if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
			return problem("Слишком часто. Попробуйте через %s.", humanDuration(apiErr.RetryAfter))
		}
		return problem("Слишком часто, подождите немного.")
	case errors.Is(err, panelclient.ErrValidation):
		var ambiguous *panelclient.AmbiguousError
		if errors.As(err, &ambiguous) {
			return problem("%q подходит нескольким серверам: %s. Уточните название.",
				ambiguous.Query, strings.Join(ambiguous.Names(), ", "))
		}
		var apiErr *panelclient.Error
		if errors.As(err, &apiErr) && apiErr.Message != "" {
			return problem("%s", apiErr.Message)
		}
		return problem("Не понял запрос.")
	default:
		// Deliberately vague: the underlying error can name internal paths and
		// addresses, and this goes into a chat other people can read. The
		// detail belongs in the daemon's log.
		return problem("Панель не ответила. Если это повторяется, посмотрите её журнал.")
	}
}

func displayName(name string) string {
	if name == "" {
		return "Сервер"
	}
	return name
}

// statusMark is the marker in front of a server's name. Text rather than an
// emoji: both platforms render it the same everywhere, and it survives being
// copied out of a chat.
func statusMark(status string) string {
	switch status {
	case panelclient.StatusRunning:
		return "[+]"
	case panelclient.StatusStarting, panelclient.StatusStopping, panelclient.StatusCreating:
		return "[~]"
	case panelclient.StatusCrashed:
		return "[!]"
	default:
		return "[ ]"
	}
}

func statusWord(status string) string {
	switch status {
	case panelclient.StatusRunning:
		return "запущен"
	case panelclient.StatusStarting:
		return "запускается"
	case panelclient.StatusStopping:
		return "останавливается"
	case panelclient.StatusStopped:
		return "остановлен"
	case panelclient.StatusCreating:
		return "создаётся"
	case panelclient.StatusCrashed:
		return "упал"
	default:
		return status
	}
}

// humanDuration renders a duration the way someone would say it.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + " с"
	}
	if d < time.Hour {
		return strconv.Itoa(int(d.Minutes())) + " мин"
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		if minutes == 0 {
			return strconv.Itoa(hours) + " ч"
		}
		return fmt.Sprintf("%d ч %d мин", hours, minutes)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if hours == 0 {
		return strconv.Itoa(days) + " дн"
	}
	return fmt.Sprintf("%d дн %d ч", days, hours)
}
