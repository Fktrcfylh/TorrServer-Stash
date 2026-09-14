package api

import (
	"net/http"
	"strings"

	"server/torrshash"

	"server/dlna"
	gstreamer "server/gstreamer/bridge"
	"server/log"
	set "server/settings"
	"server/torr"
	"server/torr/state"
	"server/web/api/utils"

	"github.com/anacrolix/torrent"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// Action: add, get, set, rem, list, drop, wipe, pin
type torrReqJS struct {
	requestI
	Link     string `json:"link,omitempty"`
	Hash     string `json:"hash,omitempty"`
	Title    string `json:"title,omitempty"`
	Category string `json:"category,omitempty"`
	Poster   string `json:"poster,omitempty"`
	Data     string `json:"data,omitempty"`
	SaveToDB bool   `json:"save_to_db,omitempty"`
	PinMode  string `json:"pin_mode,omitempty"`
	PinNext  *int   `json:"pin_next,omitempty"`
}

// torrents godoc
//
//	@Summary		Handle torrents informations
//	@Description	Allow to list, add, remove, get, set, drop, wipe, pin torrents on server. The action depends of what has been asked.
//
//	@Tags			API
//
//	@Param			request	body	torrReqJS	true	"Torrent request. Available params for action: add, get, set, rem, list, drop, wipe, pin. link required for add, hash required for get, set, rem, drop, pin. pin_mode (off, all, next) required for pin; pin_next optional (>= 0, default DefaultPinNext)."
//
//	@Accept			json
//	@Produce		json
//	@Security		BasicAuth
//	@Success		200
//	@Router			/torrents [post]
func torrents(c *gin.Context) {
	var req torrReqJS
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	switch req.Action {
	case "add":
		{
			addTorrent(req, c)
		}
	case "get":
		{
			getTorrent(req, c)
		}
	case "set":
		{
			setTorrent(req, c)
		}
	case "rem":
		{
			remTorrent(req, c)
		}
	case "list":
		{
			listTorrents(c)
		}
	case "drop":
		{
			dropTorrent(req, c)
		}
	case "wipe":
		{
			wipeTorrents(c)
		}
	case "pin":
		{
			pinTorrent(req, c)
		}
	default:
		{
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": errors.Errorf("unknown action: %q", req.Action).Error()})
		}
	}
}

func addTorrent(req torrReqJS, c *gin.Context) {
	if req.Link == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "link is empty"})
		return
	}

	log.TLogln("add torrent", req.Link)
	req.Link = strings.ReplaceAll(req.Link, "&amp;", "&")

	var torrSpec *torrent.TorrentSpec
	var torrsHash *torrshash.TorrsHash
	var err error

	if strings.HasPrefix(req.Link, "torrs://") {
		torrSpec, torrsHash, err = utils.ParseTorrsHash(req.Link)
		if err != nil {
			log.TLogln("error parse torrshash:", err)
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": errors.Errorf("error parse torrshash: %v", err).Error()})
			return
		}
		if req.Title == "" {
			req.Title = torrsHash.Title()
		}
		if req.Poster == "" {
			req.Poster = torrsHash.Poster()
		}
		if req.Category == "" {
			req.Category = torrsHash.Category()
		}
	} else {
		torrSpec, err = utils.ParseLink(req.Link)
		if err != nil {
			log.TLogln("error parse link:", err)
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": errors.Errorf("error parse link: %v", err).Error()})
			return
		}
	}

	tor, err := torr.AddTorrent(torrSpec, req.Title, req.Poster, req.Data, req.Category)
	if err != nil {
		log.TLogln("error add torrent:", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": errors.Errorf("error adding torrent: %v", err).Error()})
		return
	}

	go func() {
		if !tor.GotInfo() {
			log.TLogln("error add torrent:", "timeout connection get torrent info")
			return
		}

		if tor.Title == "" {
			tor.Title = torrSpec.DisplayName // prefer dn over name
			tor.Title = strings.ReplaceAll(tor.Title, "rutor.info", "")
			tor.Title = strings.ReplaceAll(tor.Title, "_", " ")
			tor.Title = strings.Trim(tor.Title, " ")
			if tor.Title == "" {
				tor.Title = tor.Name()
			}
		}

		if req.SaveToDB {
			torr.SaveTorrentToDB(tor)
		}
	}()

	if set.BTsets.EnableDLNA {
		dlna.Stop()
		dlna.Start()
	}
	c.JSON(200, tor.Status())
}

func getTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "hash is empty"})
		return
	}
	tor := torr.GetTorrent(req.Hash)

	if tor != nil {
		st := tor.Status()
		c.JSON(200, st)
	} else {
		c.Status(http.StatusNotFound)
	}
}

func setTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "hash is empty"})
		return
	}
	torr.SetTorrent(req.Hash, req.Title, req.Poster, req.Category, req.Data)
	c.Status(200)
}

func pinTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "hash is empty"})
		return
	}
	if set.ReadOnly {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Read-only mode"})
		return
	}
	if req.PinMode != set.PinModeOff && req.PinMode != set.PinModeAll && req.PinMode != set.PinModeNext {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": errors.Errorf("unknown pin_mode: %q", req.PinMode).Error()})
		return
	}
	if req.PinNext != nil && *req.PinNext < 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "pin_next must not be negative"})
		return
	}
	if req.PinMode != set.PinModeOff && !set.BTsets.UseDisk {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "pin requires disk storage (UseDisk)"})
		return
	}
	next := set.BTsets.DefaultPinNext
	if req.PinNext != nil {
		next = *req.PinNext
	}
	tor := torr.SetTorrentPin(req.Hash, req.PinMode, next)
	if tor == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "torrent not found"})
		return
	}
	c.JSON(200, tor.Status())
}

func remTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "hash is empty"})
		return
	}
	torr.RemTorrent(req.Hash)
	gstreamer.Remove(req.Hash)
	// TODO: remove
	if set.BTsets.EnableDLNA {
		dlna.Stop()
		dlna.Start()
	}
	c.Status(200)
}

func listTorrents(c *gin.Context) {
	list := torr.ListTorrent()
	if len(list) == 0 {
		c.JSON(200, []*state.TorrentStatus{})
		return
	}
	var stats []*state.TorrentStatus
	for _, tr := range list {
		stats = append(stats, tr.Status())
	}
	c.JSON(200, stats)
}

func dropTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "hash is empty"})
		return
	}
	torr.DropTorrent(req.Hash)
	gstreamer.Remove(req.Hash)
	c.Status(200)
}

func wipeTorrents(c *gin.Context) {
	torrents := torr.ListTorrent()
	for _, t := range torrents {
		hash := t.TorrentSpec.InfoHash.HexString()
		torr.RemTorrent(hash)
		gstreamer.Remove(hash)
	}
	// TODO: remove (copied todo from remTorrent())
	if set.BTsets.EnableDLNA {
		dlna.Stop()
		dlna.Start()
	}
	c.Status(200)
}
