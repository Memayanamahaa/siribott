package bot

import (
	"context"
	"strings"

	"github.com/bots-house/share-file-bot/core"
	"github.com/friendsofgo/errors"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/rs/zerolog/log"
)

const (
	textHelp                = "Я помогу тебе поделится любым медиафайлом (фото, видео, документы, аудио, голосовые) с подписчиками твоего канала. Отправь любой из перечисленных файлов, а я в ответ дам тебе ссылку. Так же рекомендую указать подпись, чтобы человек не забыл кто ему это пошарил 🤗.\n\n /settings - для более тонкой настройки"
	textStart               = "Привет! 👋\n\n" + textHelp
	textUnsupportedFileKind = "К сожалению, я не поддерживаю данный тип файлов. На данный момент я умею работать только с документами, видео, фото, аудио и голосовыми. Отправь и перешли мне сообщение перечисленного типа, а в ответ я дам тебе ссылку."
	mdv2                    = "MarkdownV2"
)

func (bot *Bot) onHelp(ctx context.Context, msg *tgbotapi.Message) error {
	answer := bot.newAnswerMsg(msg, textHelp)
	return bot.send(ctx, answer)
}

func (bot *Bot) onStart(ctx context.Context, msg *tgbotapi.Message) error {
	if args := msg.CommandArguments(); args != "" && !strings.HasPrefix(args, refDeepLinkPrefix) {
		user := getUserCtx(ctx)

		log.Ctx(ctx).Debug().Str("public_id", args).Msg("query file")
		result, err := bot.fileSrv.GetFileByPublicID(ctx, user, args)
		if errors.Cause(err) == core.ErrFileNotFound {
			answer := bot.newAnswerMsg(msg, "😐Ничего не знаю о таком файле, проверь ссылку...")
			return bot.send(ctx, answer)
		} else if err != nil {
			return errors.Wrap(err, "download file")
		}

		switch {
		case result.OwnedFile != nil:
			return bot.send(ctx, bot.renderOwnedFile(msg, result.OwnedFile))
		case result.File != nil:
			return bot.send(ctx, bot.renderNotOwnedFile(msg, result.File))
		case result.ChatSubRequest != nil:
			return bot.send(ctx, bot.renderSubRequest(msg, result.ChatSubRequest))
		default:
			log.Ctx(ctx).Error().Msg("bad result")
		}
	}

	answer := bot.newAnswerMsg(msg, textStart)
	return bot.send(ctx, answer)
}

func (bot *Bot) onUnsupportedFileKind(ctx context.Context, msg *tgbotapi.Message) error {
	answer := bot.newReplyMsg(msg, textUnsupportedFileKind)
	return bot.send(ctx, answer)
}

func (bot *Bot) onVersion(ctx context.Context, msg *tgbotapi.Message) error {
	answer := bot.newReplyMsg(msg, "`"+bot.revision+"`")
	return bot.send(ctx, answer)
}
