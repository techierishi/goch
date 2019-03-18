package chat

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/gorilla/mux"

	"github.com/ribice/goch"

	"github.com/ribice/msv/bind"
	"github.com/ribice/msv/render"
)

var (
	exceedsAny func(map[string]goch.Limit) error
	exceeds    func(string, goch.Limit) error
	alfaRgx    *regexp.Regexp
	mailRgx    *regexp.Regexp
)

// Limiter represents chat service limit checker
type Limiter interface {
	Exceeds(string, goch.Limit) error
	ExceedsAny(map[string]goch.Limit) error
}

// New creates new websocket api
func New(m *mux.Router, store Store, l Limiter, authMW mux.MiddlewareFunc) *API {
	api := API{
		store: store,
	}

	exceeds = l.Exceeds
	alfaRgx = regexp.MustCompile("^[a-zA-Z0-9_]*$")
	mailRgx = regexp.MustCompile("^[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$")

	sr := m.PathPrefix("/channels").Subrouter()

	sr.HandleFunc("", api.listChannels).Methods("GET")
	sr.HandleFunc("/register", api.registerNick).Methods("POST")
	sr.HandleFunc("/{name}", api.listMembers).Methods("GET").Queries("secret", "{[a-zA-Z0-9_]*$}")

	ar := m.PathPrefix("/admin").Subrouter()
	ar.Use(authMW)
	ar.HandleFunc("", api.createChannel).Methods("POST")
	ar.HandleFunc("/{name}/user/{uid}", api.unreadCount).Methods("GET")
	return &api
}

// API represents websocket api service
type API struct {
	store Store
}

// Store represents chat store interface
type Store interface {
	Save(*goch.Chat) error
	Get(string) (*goch.Chat, error)
	ListChannels() ([]string, error)
	GetUnreadCount(string, string) uint64
}

type createReq struct {
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
}

func (cr *createReq) Bind() error {
	if !alfaRgx.MatchString(cr.Name) {
		return errors.New("name must contain only alphanumeric and underscores")
	}
	return exceeds(cr.Name, goch.ChanLimit)
}

func (api *API) createChannel(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := bind.JSON(w, r, &req); err != nil {
		return
	}
	ch := goch.NewChannel(req.Name, req.IsPrivate)
	if err := api.store.Save(ch); err != nil {
		http.Error(w, fmt.Sprintf("could not create channel: %v", err), 500)
		return
	}
	render.JSON(w, r, ch.Secret)
}

type registerReq struct {
	UID           string `json:"uid"`
	DisplayName   string `json:"display_name"`
	Email         string `json:"email"`
	Secret        string `json:"secret"`
	Channel       string `json:"channel"`
	ChannelSecret string `json:"channel_secret"`
}

type registerResp struct {
	Secret string `json:"secret"`
}

func (r *registerReq) Bind() error {
	if !alfaRgx.MatchString(r.UID) {
		return errors.New("uid must contain only alphanumeric and underscores")
	}
	if !alfaRgx.MatchString(r.Secret) {
		return errors.New("secret must contain only alphanumeric and underscores")
	}
	if !mailRgx.MatchString(r.Email) {
		return errors.New("invalid email address")
	}

	return exceedsAny(map[string]goch.Limit{
		r.UID:           goch.UIDLimit,
		r.DisplayName:   goch.DisplayNameLimit,
		r.ChannelSecret: goch.ChanSecretLimit,
		r.Secret:        goch.SecretLimit,
		r.Channel:       goch.ChanLimit,
	})
}

func (api *API) registerNick(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := bind.JSON(w, r, &req); err != nil {
		return
	}
	ch, err := api.store.Get(req.Channel)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid secret or unexisting channel: %v", err), 500)
		return
	}

	if ch.Secret != req.ChannelSecret {
		http.Error(w, fmt.Sprintf("invalid secret or unexisting channel: %v", err), 500)
		return
	}

	secret, err := ch.Register(&goch.User{
		UID:         req.UID,
		DisplayName: req.DisplayName,
		Email:       req.Email,
	}, req.Secret)

	if err != nil {
		http.Error(w, fmt.Sprintf("error registering to channel: %v", err), 500)
		return
	}

	if err = api.store.Save(ch); err != nil {
		ch.Leave(req.UID)
		http.Error(w, fmt.Sprintf("could not update channel membership: %v", err), 500)
		return
	}

	render.JSON(w, r, registerResp{secret})

}

type unreadCountResp struct {
	Count uint64 `json:"count"`
}

func (api *API) unreadCount(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uid, chanName := vars["uid"], vars["name"]
	if err := exceedsAny(map[string]goch.Limit{
		chanName: goch.UIDLimit,
		uid:      goch.ChanLimit,
	}); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	uc := api.store.GetUnreadCount(uid, chanName)
	render.JSON(w, r, &unreadCountResp{uc})

}

func (api *API) listMembers(w http.ResponseWriter, r *http.Request) {

	chanName := r.URL.Query().Get("name")
	secret := mux.Vars(r)["secret"]

	if err := exceedsAny(map[string]goch.Limit{
		chanName: goch.ChanLimit,
		secret:   goch.ChanSecretLimit,
	}); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	ch, err := api.store.Get(chanName)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid secret or unexisting channel: %v", err), 500)
		return
	}

	if ch.Secret != secret {
		http.Error(w, fmt.Sprintf("invalid secret or unexisting channel: %v", err), 500)
		return
	}

	render.JSON(w, r, ch.ListMembers())
}

func (api *API) listChannels(w http.ResponseWriter, r *http.Request) {
	chans, err := api.store.ListChannels()
	if err != nil {
		http.Error(w, fmt.Sprintf("unable to fetch channels: %v", err), 500)
		return
	}
	render.JSON(w, r, chans)

}
