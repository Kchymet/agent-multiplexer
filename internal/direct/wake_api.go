package direct

import (
	"errors"
	"net/http"

	amuxstore "amux/internal/store"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Direct wake API (docs/direct-wake-contract.md §6.3, §8). GET/POST only, so
// the existing CORS preflight covers it; route ids travel in JSON bodies only,
// never in a URL. Every route needs a live grant with read and nothing more.

const (
	PathWakeSubscribe     = "/direct/v1/wake/subscribe"
	PathWakeUnsubscribe   = "/direct/v1/wake/unsubscribe"
	PathWakeSubscriptions = "/direct/v1/wake/subscriptions"
	PathWakeWatch         = "/direct/v1/wake/watch"
	PathWakeEvents        = "/direct/v1/wake/events"
	PathWakeAck           = "/direct/v1/wake/ack"
)

// Wake request and response bodies.
type (
	WakeSubscribeRequest struct {
		RouteID          string `json:"route_id"`
		ActivationSecret string `json:"activation_secret"`
	}
	WakeRouteRequest struct {
		RouteID string `json:"route_id"`
	}
	WakeUnsubscribeResponse struct {
		Removed bool `json:"removed"`
	}
	WakeSubscriptionsRequest  struct{}
	WakeSubscriptionsResponse struct {
		Subscriptions []WakeSubscription `json:"subscriptions"`
	}
	WakeSubscription struct {
		WakeSubView
		Watches []WakeWatch `json:"watches"`
	}
	WakeWatchRequest struct {
		RouteID     string `json:"route_id"`
		SessionID   string `json:"session_id"`
		Incarnation string `json:"incarnation"`
		Watch       bool   `json:"watch"`
	}
	WakeWatchResponse struct {
		RouteID     string `json:"route_id"`
		SessionID   string `json:"session_id"`
		Incarnation string `json:"incarnation"`
		Watched     bool   `json:"watched"`
	}
	WakeAckRequest struct {
		RouteID string        `json:"route_id"`
		Items   []WakeAckItem `json:"items"`
	}
	WakeAckResponse struct {
		Acked int `json:"acked"`
	}
	// WakeCapabilities is GET /direct/v1/capabilities `wake`.
	WakeCapabilities struct {
		V        int  `json:"v"`
		Enabled  bool `json:"enabled"`
		Degraded bool `json:"degraded"`
	}
)

func (s *Server) registerWakeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+PathWakeSubscribe, s.wakeSubscribe)
	mux.HandleFunc("POST "+PathWakeUnsubscribe, s.wakeUnsubscribe)
	mux.HandleFunc("POST "+PathWakeSubscriptions, s.wakeSubscriptions)
	mux.HandleFunc("POST "+PathWakeWatch, s.wakeWatch)
	mux.HandleFunc("POST "+PathWakeEvents, s.wakeEvents)
	mux.HandleFunc("POST "+PathWakeAck, s.wakeAck)
}

// wakeAdmit admits a read lease for a wake route, refusing any query string.
func (s *Server) wakeAdmit(w http.ResponseWriter, r *http.Request, body any) *Lease {
	if r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "wake routes take no query parameters")
		return nil
	}
	if err := decodeJSON(r, w, body); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "malformed request body")
		return nil
	}
	l := s.admit(w, r, PermRead)
	if l == nil {
		return nil
	}
	if s.cfg.Wake == nil {
		l.Done()
		writeErr(w, http.StatusConflict, CodeWakeDisabled, "wake-ups are not enabled on this host")
		return nil
	}
	return l
}

func (s *Server) writeWakeErr(w http.ResponseWriter, l *Lease, err error) {
	switch {
	case errors.Is(err, errWakeDisabled):
		writeErr(w, http.StatusConflict, CodeWakeDisabled, "wake-ups are not enabled on this host")
	case errors.Is(err, errWakeAlreadyActive):
		writeErr(w, http.StatusConflict, CodeWakeAlreadyActive, "this route is already active for this client")
	case errors.Is(err, errWakeCapacity):
		writeErr(w, http.StatusConflict, CodeCapacity, "too many wake subscriptions")
	case errors.Is(err, errWakeDegraded):
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "wake-ups are temporarily unavailable on this host")
	case errors.Is(err, errBadRequest):
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "malformed wake request")
	case errors.Is(err, errAuthRevoked):
		writeErr(w, http.StatusUnauthorized, CodeAuthRevoked, "access revoked")
	case errors.Is(err, errDegraded), rolledBack(err):
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "the change could not be saved")
	default:
		s.writeBackendErr(w, l, err)
	}
}

func (s *Server) wakeSubscribe(w http.ResponseWriter, r *http.Request) {
	var req WakeSubscribeRequest
	l := s.wakeAdmit(w, r, &req)
	if l == nil {
		return
	}
	defer l.Done()
	v, err := s.cfg.Authority.WakeSubscribe(l, req.RouteID, req.ActivationSecret)
	if err != nil {
		s.writeWakeErr(w, l, err)
		return
	}
	s.cfg.Wake.poke()
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) wakeUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req WakeRouteRequest
	l := s.wakeAdmit(w, r, &req)
	if l == nil {
		return
	}
	defer l.Done()
	removed, err := s.cfg.Authority.WakeUnsubscribe(l, req.RouteID)
	if err != nil {
		s.writeWakeErr(w, l, err)
		return
	}
	writeJSON(w, http.StatusOK, WakeUnsubscribeResponse{Removed: removed})
}

func (s *Server) wakeSubscriptions(w http.ResponseWriter, r *http.Request) {
	var req WakeSubscriptionsRequest
	l := s.wakeAdmit(w, r, &req)
	if l == nil {
		return
	}
	defer l.Done()
	nonce := ""
	if auth := s.cfg.Authority.wakeAuthSnapshot(); auth != nil {
		nonce = auth.Nonce
	}
	out := WakeSubscriptionsResponse{Subscriptions: []WakeSubscription{}}
	for _, v := range s.cfg.Authority.WakeSubscriptions(l) {
		out.Subscriptions = append(out.Subscriptions, WakeSubscription{WakeSubView: v, Watches: s.cfg.Wake.store.watches(l.ClientID, v.RouteID, nonce)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) wakeWatch(w http.ResponseWriter, r *http.Request) {
	var req WakeWatchRequest
	l := s.wakeAdmit(w, r, &req)
	if l == nil {
		return
	}
	defer l.Done()
	if !hp.ValidDirectWakeRouteID(req.RouteID) || !validSessionID(req.SessionID) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "malformed wake request")
		return
	}
	nonce, ok := s.cfg.Wake.authorizedSub(l, req.RouteID)
	if !ok {
		// Unknown, foreign or another grant's route: indistinguishable.
		writeErr(w, http.StatusNotFound, CodeNotFound, "no such subscription")
		return
	}
	resp := WakeWatchResponse{RouteID: req.RouteID, SessionID: req.SessionID}
	if !req.Watch {
		_, err := s.cfg.Wake.store.setWatch(l.ClientID, req.RouteID, nonce, req.SessionID, "", false)
		if err != nil {
			s.writeWakeErr(w, l, err)
			return
		}
		_ = s.cfg.Wake.store.flush(true)
		cur, _, _ := s.cfg.Wake.sessionIncarnation(l.Context(), l.Grant, req.SessionID)
		resp.Incarnation = cur
		writeJSON(w, http.StatusOK, resp)
		return
	}
	cur, visible, err := s.cfg.Wake.sessionIncarnation(l.Context(), l.Grant, req.SessionID)
	if err != nil {
		s.writeBackendErr(w, l, err)
		return
	}
	if !visible {
		writeErr(w, http.StatusNotFound, CodeNotFound, "no such session")
		return
	}
	if cur == "" || cur != req.Incarnation {
		writeJSON(w, http.StatusConflict, struct {
			APIError
			Incarnation string `json:"incarnation"`
		}{APIError{Code: CodeWakeIncarnation, Error: "the session instance changed"}, cur})
		return
	}
	watched, err := s.cfg.Wake.store.setWatch(l.ClientID, req.RouteID, nonce, req.SessionID, cur, true)
	if err != nil {
		s.writeWakeErr(w, l, err)
		return
	}
	_ = s.cfg.Wake.store.flush(true)
	resp.Incarnation, resp.Watched = cur, watched
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) wakeEvents(w http.ResponseWriter, r *http.Request) {
	var req WakeFetchRequest
	l := s.wakeAdmit(w, r, &req)
	if l == nil {
		return
	}
	defer l.Done()
	if !hp.ValidDirectWakeRouteID(req.RouteID) || req.AfterSeq < 0 || req.Limit < 0 || req.Limit > wakeFetchMax ||
		req.SnapshotHW < 0 || req.RecoveredThrough < 0 ||
		(!req.Snapshot && (req.SnapshotHW != 0 || req.RecoveredThrough != 0)) ||
		(req.Snapshot && req.SnapshotHW != 0 && req.AfterSeq > req.SnapshotHW) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "malformed wake request")
		return
	}
	if auth := s.cfg.Authority.wakeAuthSnapshot(); auth == nil {
		writeErr(w, http.StatusConflict, CodeWakeDisabled, "wake-ups are not enabled on this host")
		return
	}
	nonce, ok := s.cfg.Wake.authorizedSub(l, req.RouteID)
	if !ok {
		writeJSON(w, http.StatusOK, WakeFetchResponse{RouteID: req.RouteID, Items: []WakeItem{}, NextSeq: req.AfterSeq})
		return
	}
	vis, err := s.cfg.Wake.visibility(l.Context(), l.Grant)
	if err != nil {
		s.writeBackendErr(w, l, err)
		return
	}
	resp, err := s.cfg.Wake.store.fetch(l.ClientID, req.RouteID, nonce, req, vis, s.cfg.Now().UnixMilli())
	if err != nil {
		s.writeWakeErr(w, l, err)
		return
	}
	_ = s.cfg.Wake.store.flush(false)
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) wakeAck(w http.ResponseWriter, r *http.Request) {
	var req WakeAckRequest
	l := s.wakeAdmit(w, r, &req)
	if l == nil {
		return
	}
	defer l.Done()
	if !hp.ValidDirectWakeRouteID(req.RouteID) || len(req.Items) == 0 || len(req.Items) > wakeFetchMax {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "malformed wake request")
		return
	}
	for _, it := range req.Items {
		if !hp.ValidDirectWakeID(it.WakeID) || it.Seq <= 0 {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "malformed wake request")
			return
		}
	}
	nonce, ok := s.cfg.Wake.authorizedSub(l, req.RouteID)
	if !ok {
		writeJSON(w, http.StatusOK, WakeAckResponse{})
		return
	}
	n, err := s.cfg.Wake.store.ack(l.ClientID, req.RouteID, nonce, req.Items, s.cfg.Now().UnixMilli())
	if err != nil {
		s.writeWakeErr(w, l, err)
		return
	}
	_ = s.cfg.Wake.store.flush(false)
	writeJSON(w, http.StatusOK, WakeAckResponse{Acked: n})
}

// withIncarnations stamps each projected row with its store incarnation (best
// effort: a lookup failure leaves the field absent).
func (s *Server) withIncarnations(l *Lease, rows []Session) []Session {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	for i := 0; i < len(ids); i += amuxstore.MaxIdentityLookup {
		j := min(i+amuxstore.MaxIdentityLookup, len(ids))
		got, err := s.cfg.Backend.SessionIdentities(l.Context(), ids[i:j])
		if err != nil {
			return rows
		}
		for k := i; k < j; k++ {
			rows[k].Incarnation = got[rows[k].ID].Incarnation
		}
	}
	return rows
}
