package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/bots-house/share-file-bot/bot/state"
	"github.com/bots-house/share-file-bot/core"
	"github.com/bots-house/share-file-bot/pkg/tg"
	"github.com/bots-house/share-file-bot/service"
	"github.com/fatih/structs"
	"github.com/friendsofgo/errors"
	"github.com/getsentry/sentry-go"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/rs/zerolog/log"
	"github.com/tomasen/realip"
)

type Bot struct {
	revision string

	client *tgbotapi.BotAPI
	state  state.Store

	authSrv  *service.Auth
	fileSrv  *service.File
	adminSrv *service.Admin
	chatSrv  *service.Chat

	handler tg.Handler
}

func (bot *Bot) Self() tgbotapi.User {
	return bot.client.Self
}

func New(revision string, client *tgbotapi.BotAPI, state state.Store, authSrv *service.Auth, docSrv *service.File, adminSrv *service.Admin, chatSrv *service.Chat) (*Bot, error) {

	bot := &Bot{
		revision: revision,
		client:   client,
		state:    state,

		authSrv:  authSrv,
		fileSrv:  docSrv,
		adminSrv: adminSrv,
		chatSrv:  chatSrv,
	}

	// bot.client.Debug = true

	bot.initHandler()

	return bot, nil
}

func (bot *Bot) SetWebhookIfNeed(ctx context.Context, u string) error {
	webhook, err := bot.client.GetWebhookInfo()
	if err != nil {
		return errors.Wrap(err, "get webhook info")
	}

	if webhook.URL != u {
		u, err := url.Parse(u)
		if err != nil {
			return errors.Wrap(err, "invalid provided webhook url")
		}

		log.Ctx(ctx).Info().Str("old", webhook.URL).Str("new", u.String()).Msg("update bot webhook")
		if _, err := bot.client.SetWebhook(tgbotapi.WebhookConfig{
			URL:            u,
			MaxConnections: 40,
		}); err != nil {
			return errors.Wrap(err, "update webhook")
		}
	}

	return nil
}

func (bot *Bot) initHandler() {
	authMiddleware := newAuthMiddleware(bot.authSrv)

	handler := authMiddleware(tg.HandlerFunc(bot.onUpdate))

	bot.handler = handler
}

var (
	cbqFileRefresh               = regexp.MustCompile(`^file:(\d+):refresh$`)
	cbqFileDelete                = regexp.MustCompile(`^file:(\d+):delete$`)
	cbqFileDeleteConfirm         = regexp.MustCompile(`^file:(\d+):delete:confirm$`)
	cbqFileRestrictions          = regexp.MustCompile(`^file:(\d+):restrictions$`)
	cbqFileRestrictionsChat      = regexp.MustCompile(`file:(\d+):restrictions:chat-subscription:(\d+):toggl`)
	cbqFileRestrictionsChatCheck = regexp.MustCompile(`^file:(\d+):restrictions:chat:check$`)

	cbqSettings              = regexp.MustCompile(`^` + callbackSettings + `$`)
	cbqSettingsToggleLongIDs = regexp.MustCompile(`^` + callbackSettingsLongIDs + `$`)

	cbqSettingsChannelsAndChats              = regexp.MustCompile(`^` + callbackSettingsChannelsAndChats + `$`)
	cbqSettingsChannelsAndChatsConnect       = regexp.MustCompile(`^` + callbackSettingsChannelsAndChatsConnect + `$`)
	cbqSettingsChannelsAndChatsDetails       = regexp.MustCompile(`^settings:channels-and-chats:(\d+)$`)
	cbqSettingsChannelsAndChatsDelete        = regexp.MustCompile(`^settings:channels-and-chats:(\d+):delete$`)
	cbqSettingsChannelsAndChatsDeleteConfirm = regexp.MustCompile(`^settings:channels-and-chats:(\d+):delete:confirm$`)
)

// i known, we should rewrite it
// nolint:gocyclo
func (bot *Bot) onUpdate(ctx context.Context, update *tgbotapi.Update) error {

	if msg := update.ChannelPost; msg != nil {
		if msg.NewChatTitle != "" {
			return bot.onChatNewTitle(ctx, msg)
		}
	}

	user := getUserCtx(ctx)

	// handle message
	if msg := update.Message; msg != nil {

		userState, err := bot.state.Get(ctx, user.ID)
		if err != nil {
			return errors.Wrap(err, "get state")
		}

		//nolint:gocritic
		switch userState {
		case state.SettingsChannelsAndChatsConnect:
			return bot.onSettingsChannelsAndChatsConnectState(ctx, msg)
		}

		// handle command
		switch msg.Command() {
		case "start":
			return bot.onStart(ctx, msg)
		case "help":
			return bot.onHelp(ctx, msg)
		case "admin":
			return bot.onAdmin(ctx, msg)
		case "settings":
			return bot.onSettings(ctx, msg)
		case "version":
			return bot.onVersion(ctx, msg)
		}

		// handle other
		if kind := bot.detectKind(msg); kind != core.KindUnknown {
			return bot.onFile(ctx, msg)
		}

		return bot.onUnsupportedFileKind(ctx, msg)
	}

	// handle callback queries
	//nolint:nestif
	if cbq := update.CallbackQuery; cbq != nil {
		data := cbq.Data
		switch {

		// file check chat subscription
		case len(cbqFileRestrictionsChatCheck.FindStringIndex(data)) > 0:
			result := cbqFileRestrictionsChatCheck.FindStringSubmatch(data)

			id, err := strconv.Atoi(result[1])
			if err != nil {
				return errors.Wrap(err, "parse cbq data")
			}

			return bot.onFileRestrictionsChatCheck(ctx, cbq, core.FileID(id))

		// file menu
		case len(cbqFileRefresh.FindStringIndex(data)) > 0:
			result := cbqFileRefresh.FindStringSubmatch(data)

			id, err := strconv.Atoi(result[1])
			if err != nil {
				return errors.Wrap(err, "parse cbq data")
			}

			return bot.onFileRefreshCBQ(ctx, cbq, id)

		// file menu / delete
		case len(cbqFileDelete.FindStringIndex(data)) > 0:
			result := cbqFileDelete.FindStringSubmatch(data)

			id, err := strconv.Atoi(result[1])
			if err != nil {
				return errors.Wrap(err, "parse cbq data")
			}

			return bot.onFileDeleteCBQ(ctx, cbq, id)
		// file menu / delete
		case len(cbqFileDeleteConfirm.FindStringIndex(data)) > 0:
			result := cbqFileDeleteConfirm.FindStringSubmatch(data)

			id, err := strconv.Atoi(result[1])
			if err != nil {
				return errors.Wrap(err, "parse cbq data")
			}

			return bot.onFileDeleteConfirmCBQ(ctx, cbq, id)
		// file menu / restrictions
		case len(cbqFileRestrictions.FindStringIndex(data)) > 0:
			result := cbqFileRestrictions.FindStringSubmatch(data)

			id, err := strconv.Atoi(result[1])
			if err != nil {
				return errors.Wrap(err, "parse cbq data")
			}

			return bot.onFileRestrictionsCBQ(ctx, cbq, id)
		// file menu / restrictions / set chat
		case len(cbqFileRestrictionsChat.FindStringIndex(data)) > 0:
			result := cbqFileRestrictionsChat.FindStringSubmatch(data)

			fileID, err := strconv.Atoi(result[1])
			if err != nil {
				return errors.Wrap(err, "parse cbq data (file_id)")
			}

			chatID, err := strconv.Atoi(result[2])
			if err != nil {
				return errors.Wrap(err, "parse cbq data (chat_id)")
			}

			return bot.onFileRestrictionsSetChatCBQ(ctx, cbq,
				core.FileID(fileID),
				core.ChatID(chatID),
			)

		// settings
		case len(cbqSettings.FindStringIndex(data)) > 0:
			return bot.onSettingsCallbackQuery(ctx, cbq)

		// settings / long ids
		case len(cbqSettingsToggleLongIDs.FindStringIndex(data)) > 0:
			return bot.onSettingsToggleLongIDsCBQ(ctx, cbq)

		// settings / channels and chats
		case len(cbqSettingsChannelsAndChats.FindStringIndex(data)) > 0:
			return bot.onSettingsChannelsAndChats(ctx, cbq)

		// settings / channels and chats / connect
		case len(cbqSettingsChannelsAndChatsConnect.FindStringIndex(data)) > 0:
			return bot.onSettingsChannelsAndChatsConnect(ctx, cbq)

		// settings / channels and chats / details
		case len(cbqSettingsChannelsAndChatsDetails.FindStringIndex(data)) > 0:
			result := cbqSettingsChannelsAndChatsDetails.FindStringSubmatch(data)

			id, err := strconv.Atoi(result[1])
			if err != nil {
				return errors.Wrap(err, "parse cbq data")
			}

			return bot.onSettingsChannelsAndChatsDetails(ctx, user, cbq, core.ChatID(id))
		// settings / channels and chats / delete
		case len(cbqSettingsChannelsAndChatsDelete.FindStringIndex(data)) > 0:
			result := cbqSettingsChannelsAndChatsDelete.FindStringSubmatch(data)

			id, err := strconv.Atoi(result[1])
			if err != nil {
				return errors.Wrap(err, "parse cbq data")
			}

			return bot.onSettingsChannelsAndChatsDelete(ctx, user, cbq, core.ChatID(id))
		// settings / channels and chats / delete / confirm
		case len(cbqSettingsChannelsAndChatsDeleteConfirm.FindStringIndex(data)) > 0:
			result := cbqSettingsChannelsAndChatsDeleteConfirm.FindStringSubmatch(data)

			id, err := strconv.Atoi(result[1])
			if err != nil {
				return errors.Wrap(err, "parse cbq data")
			}

			return bot.onSettingsChannelsAndChatsDeleteConfirm(ctx, user, cbq, core.ChatID(id))
		default:
			// spew.Dump(cbq)
		}

	}

	return nil
}

func (bot *Bot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := realip.FromRequest(r)

	if !isTelegramIP(ip) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	update := &tgbotapi.Update{}

	// parse update
	if err := json.NewDecoder(r.Body).Decode(update); err != nil {
		http.Error(w, fmt.Sprintf("invalid payload: %v", err), http.StatusBadRequest)
		return
	}

	// handle update
	if err := bot.handler.HandleUpdate(ctx, update); err != nil {
		bot.onError(ctx, update, err)
		return
	}
}

func (bot *Bot) onError(ctx context.Context, update *tgbotapi.Update, er error) {
	log.Ctx(ctx).Error().Int("update_id", update.UpdateID).Err(er).Msg("handle update failed")

	sentry.AddBreadcrumb(&sentry.Breadcrumb{
		Message:  "Update",
		Level:    sentry.LevelInfo,
		Data:     structs.Map(update),
		Category: "bot",
	})

	sentry.CaptureException(er)
}
