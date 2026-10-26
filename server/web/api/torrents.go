package api

import (
	"fmt"
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

// Action: add, get, set, rem, list, drop, wipe, remote
type torrReqJS struct {
	requestI
	Link     string `json:"link,omitempty"`
	Hash     string `json:"hash,omitempty"`
	Title    string `json:"title,omitempty"`
	Category string `json:"category,omitempty"`
	Poster   string `json:"poster,omitempty"`
	Data     string `json:"data,omitempty"`
	SaveToDB bool   `json:"save_to_db,omitempty"`
	// Required for action remote: true adds the torrent to the remote
	// downloader, false removes it there with its data.
	// A pointer: a missing key is rejected, it never means false.
	RemoteDownload *bool `json:"remote_download,omitempty"`
}

// torrents godoc
//
//	@Summary		Handle torrents informations
//	@Description	Allow to list, add, remove, get, set, drop, wipe torrents on server, and to add a torrent to / remove it from the remote downloader (remote). The action depends of what has been asked.
//	@Description	remote: synchronous; remote_download=true runs torrent-add, false runs torrent-remove with its data (nothing is sent when the torrent is not flagged). The flag is saved only on success.
//	@Description	rem/wipe of a torrent flagged for the remote downloader remove it there first; on failure the torrent is kept and an error is returned.
//
//	@Tags			API
//
//	@Param			request	body	torrReqJS	true	"Torrent request. Available params for action: add, get, set, rem, list, drop, wipe, remote. link required for add, hash required for get, set, rem, drop, remote; remote_download required for remote."
//
//	@Accept			json
//	@Produce		json
//	@Security		BasicAuth
//	@Success		200	{object}	state.TorrentStatus	"add, get, remote: torrent status"
//	@Failure		400	{object}	map[string]string	"bad request, or remote downloader not configured"
//	@Failure		403	{object}	map[string]string	"remote: read-only DB mode"
//	@Failure		404	{object}	map[string]string	"remote: torrent not found"
//	@Failure		409	{object}	map[string]string	"remote: torrent not in DB and its info is not loaded yet"
//	@Failure		502	{object}	map[string]string	"remote, rem, wipe: remote downloader RPC failed"
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
	case "remote":
		{
			remoteTorrent(req, c)
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

func remTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "hash is empty"})
		return
	}
	if err := torr.RemTorrent(req.Hash); err != nil {
		c.AbortWithStatusJSON(remoteErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
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
	var failed int
	var firstErr error
	for _, t := range torrents {
		hash := t.TorrentSpec.InfoHash.HexString()
		if err := torr.RemTorrent(hash); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		gstreamer.Remove(hash)
	}
	// TODO: remove (copied todo from remTorrent())
	if set.BTsets.EnableDLNA {
		dlna.Stop()
		dlna.Start()
	}
	if failed > 0 {
		log.TLogln("API wipe: torrents not removed:", failed, "first error:", firstErr)
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("%d torrent(s) not removed: %v", failed, firstErr)})
		return
	}
	c.Status(200)
}

func remoteTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "hash is empty"})
		return
	}
	if req.RemoteDownload == nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "remote_download is required"})
		return
	}
	tor, err := torr.SetTorrentRemote(req.Hash, *req.RemoteDownload)
	if err != nil {
		c.AbortWithStatusJSON(remoteErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tor.Status())
}

// remoteErrorStatus maps errors of remote downloader writes to HTTP statuses;
// anything not recognised is an RPC failure of the client (502).
func remoteErrorStatus(err error) int {
	switch {
	case errors.Is(err, torr.ErrRemoteConfig):
		return http.StatusBadRequest
	case errors.Is(err, torr.ErrTorrentNotFound):
		return http.StatusNotFound
	case errors.Is(err, torr.ErrReadOnly):
		return http.StatusForbidden
	case errors.Is(err, torr.ErrInfoPending):
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}
