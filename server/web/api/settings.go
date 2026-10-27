package api

import (
	"net/http"

	"server/bonjour"
	"server/dlna"
	"server/rutor"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"

	"server/log"
	"server/remotedl"
	sets "server/settings"
	"server/torr"
)

// Action: get, set, def, test_remote
type setsReqJS struct {
	requestI
	Sets *sets.BTSets `json:"sets,omitempty"`
	// RemoteDownloader is an optional unsaved config for test_remote; the
	// saved one is tested when it is absent.
	RemoteDownloader *sets.RemoteDownloaderConfig `json:"remote_downloader,omitempty"`
}

// settings godoc
//
//	@Summary		Get / Set server settings
//	@Description	Allow to get or set server settings.
//
//	@Tags			API
//
//	@Param			request	body	setsReqJS	true	"Settings request. Available params for action: get, set, def, test_remote. test_remote checks the remote downloader in remote_downloader (or the saved one) and returns remotedl.SessionInfo; 400 on bad config, 502 when the client fails."
//
//	@Accept			json
//	@Produce		json
//	@Security		BasicAuth
//	@Success		200	{object}	sets.BTSets	"Settings JSON or nothing. Depends on what action has been asked."
//	@Router			/settings [post]
func settings(c *gin.Context) {
	var req setsReqJS
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	if req.Action == "get" {
		c.JSON(200, sets.BTsets)
		return
	} else if req.Action == "set" {
		torr.SetSettings(req.Sets)
		restartServices()
		c.Status(200)
		return
	} else if req.Action == "test_remote" {
		testRemote(c, req.RemoteDownloader)
		return
	} else if req.Action == "def" {
		torr.SetDefSettings()
		restartServices()
		c.Status(200)
		return
	}
	c.AbortWithError(http.StatusBadRequest, errors.New("action is empty"))
}

// testRemote runs session-get against cfg, or the saved remote downloader when
// cfg is nil. It never saves settings.
func testRemote(c *gin.Context, cfg *sets.RemoteDownloaderConfig) {
	source := "body"
	if cfg == nil {
		source = "saved"
		cfg = &sets.BTsets.RemoteDownloader
	}
	d, err := remotedl.New(*cfg)
	if err != nil {
		// cfg.URL is not logged here: a rejected URL may carry credentials.
		log.TLogln("API test_remote: config error, source:", source, "type:", cfg.Type, "err:", err)
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	info, err := d.Session(c.Request.Context())
	if err != nil {
		log.TLogln("API test_remote: failed, source:", source, "type:", cfg.Type, "url:", cfg.URL, "err:", err)
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	log.TLogln("API test_remote: ok, source:", source, "url:", cfg.URL, "version:", info.Version, "rpc-version:", info.RPCVersion, "peer-port:", info.PeerPort, "lpd-enabled:", info.LPDEnabled)
	c.JSON(http.StatusOK, info)
}

// restartServices applies the settings now in effect to DLNA, Bonjour and the
// rutor search.
func restartServices() {
	dlna.Stop()
	if sets.BTsets.EnableDLNA {
		dlna.Start()
	}
	bonjour.Stop()
	if sets.BTsets.EnableBonjour {
		bonjour.Start()
	}
	rutor.Stop()
	rutor.Start()
}
