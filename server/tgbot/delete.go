package tgbot

import (
	"fmt"

	tele "gopkg.in/telebot.v4"
	"server/torr"
)

// deleteTorrent removes the torrent of a delete button; the error of a failed
// removal (remote downloader) is returned and the torrent kept.
func deleteTorrent(c tele.Context) error {
	args := c.Args()
	if len(args) < 2 {
		return nil
	}
	hash := args[1]
	if !isHash(hash) {
		return nil
	}
	return torr.RemTorrent(hash)
}

func clear(c tele.Context) error {
	torrents := torr.ListTorrent()
	count := len(torrents)
	if count == 0 {
		return c.Send(tr(c.Sender().ID, "no_torrents"))
	}
	uid := c.Sender().ID
	btnYes := tele.InlineButton{Text: tr(uid, "btn_yes"), Unique: "fclear", Data: "1"}
	btnNo := tele.InlineButton{Text: tr(uid, "btn_no"), Unique: "fclear", Data: "0"}
	kbd := &tele.ReplyMarkup{InlineKeyboard: [][]tele.InlineButton{{btnYes, btnNo}}}
	return c.Send(fmt.Sprintf(tr(uid, "clear_confirm"), count), kbd)
}

func clearConfirm(c tele.Context, confirm string) error {
	uid := c.Sender().ID
	if confirm != "1" {
		_ = c.Respond(&tele.CallbackResponse{Text: tr(uid, "canceled")})
		return c.Bot().Delete(c.Callback().Message)
	}
	torrents := torr.ListTorrent()
	count := 0
	var firstErr error
	for _, t := range torrents {
		if err := torr.RemTorrent(t.Hash().HexString()); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		count++
	}
	_ = c.Respond(&tele.CallbackResponse{Text: tr(uid, "deleted")})
	_ = c.Bot().Delete(c.Callback().Message)
	if err := c.Send(fmt.Sprintf(tr(uid, "clear_done"), count)); err != nil {
		return err
	}
	if firstErr != nil {
		return c.Send(tr(uid, "error")+":\n"+escapeHtml(firstErr.Error()), tele.ModeHTML)
	}
	return nil
}
