package direct

import (
	"context"
	"errors"
	"net/http"

	"amux/internal/core"
	"amux/internal/hostbackend"
	amuxstore "amux/internal/store"
)

// creation_identity.go keeps a replayed creation from ever sending a client to
// an unrelated session. A creation's ids are only meaningful as the exact
// sessions the daemon created: the store may later delete either one and
// reuse its id (or be replaced or restored). So the server records the
// created session's and root's store incarnations with the result
// (Authority.FinishCreated), and every success replay — a synchronous POST, an
// asynchronous POST and GET PathCreations — reads the current identities first
// (verifyCreated) and answers with the ids only when both still match:
//
//   - both match (archived or not): the recorded success, unchanged;
//   - either is missing, has another incarnation, or the member now belongs
//     to a different root: created_target_unavailable, without ids;
//   - the record has no incarnations (written before they existed, or an
//     older binary dropped them): created_identity_unverifiable, without ids;
//   - the identities cannot be read now (a transient failure, or a daemon
//     without identities): 503 created_identity_unavailable, never
//     unavailable (a "did not run" code); the record is left exactly as it
//     was and the same request may be retried.
//
// Neither new code is a "did not run" answer: the creation did run. A client
// never retries either automatically with a new idem_key.
//
// Incarnations never leave the host: they travel only on the daemon's host
// socket and in the direct store, never in an ActionResponse,
// CreationStatus, inventory row or log line.

// admission is what a fresh destination precheck establishes: the sessions an
// archive changes and, for add-agent, the incarnation of the target root the
// member must be created under.
type admission struct {
	affected []string
	rootInc  string
}

// identityVerdict is verifyCreated's answer for one success record.
type identityVerdict int

const (
	identityMatches identityVerdict = iota
	identityTargetUnavailable
	identityUnverifiable
)

// isCreationSuccess reports a done, successful record that names a created
// session (only creation verbs record NewID).
func isCreationSuccess(r *idemRecord) bool {
	return r != nil && r.State == idemDone && r.OK && r.NewID != ""
}

// verifyCreated checks a successful creation record against the store's
// current identities. An error means the identities could not be read; it
// never changes the record.
func (s *Server) verifyCreated(ctx context.Context, r idemRecord) (identityVerdict, error) {
	if r.NewIncarnation == "" || r.RootIncarnation == "" || r.RootID == "" {
		return identityUnverifiable, nil
	}
	ids := []string{r.NewID}
	if r.RootID != r.NewID {
		ids = append(ids, r.RootID)
	}
	cur, err := s.cfg.Backend.SessionIdentities(ctx, ids)
	if err != nil {
		return 0, err
	}
	n, okN := cur[r.NewID]
	root, okR := cur[r.RootID]
	wantParent := r.RootID // a member's root
	if r.NewID == r.RootID {
		wantParent = "" // a root has none
	}
	if !okN || !okR || n.Incarnation != r.NewIncarnation || root.Incarnation != r.RootIncarnation ||
		root.RootID != "" || n.RootID != wantParent {
		return identityTargetUnavailable, nil
	}
	return identityMatches, nil
}

// Fixed texts of the identity refusals. They name no id.
const (
	targetUnavailableMsg = "the session this creation made, or its workgroup, no longer exists as created (deleted, or its id now names another session); the creation ran, and it will not run again with this idem_key"
	unverifiableMsg      = "this creation was recorded before the server kept session identities, so it cannot be confirmed that its ids still name the sessions it created; it will not run again with this idem_key"
	identityReadMsg      = "could not confirm the created session right now; the record is unchanged, retry the same request"
)

// identityReadErr answers a failure to read identities for a replay. The
// creation already ran, so the answer is never unavailable (which clients
// read as "did not run" and may retry with a fresh key): it is the
// replay-specific created_identity_unavailable, 503, and the record is left
// untouched so the same request may be retried.
func identityReadErr(w http.ResponseWriter, err error) {
	msg := identityReadMsg
	if errors.Is(err, hostbackend.ErrUpgradeRequired) {
		msg = "the amux daemon no longer supports session identities, so this recorded creation cannot be confirmed; upgrade and restart the daemon, then retry the same request (the record is unchanged)"
	}
	writeErr(w, http.StatusServiceUnavailable, CodeCreatedIdentityUnavailable, msg)
}

// replayCreated answers a synchronous replay of a successful creation.
func (s *Server) replayCreated(w http.ResponseWriter, l *Lease, rec *idemRecord) {
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended")
		return
	}
	verdict, err := s.verifyCreated(l.Context(), *rec)
	// Access may have ended during the lookup (which then fails): that is the
	// answer, not a misleading 503.
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended")
		return
	}
	if err != nil {
		s.cfg.Logf("direct: verify created session: %v", err)
		identityReadErr(w, err)
		return
	}
	switch verdict {
	case identityTargetUnavailable:
		writeErr(w, http.StatusConflict, CodeCreatedTargetUnavailable, targetUnavailableMsg)
	case identityUnverifiable:
		writeErr(w, http.StatusConflict, CodeCreatedIdentityUnverifiable, unverifiableMsg)
	default:
		writeJSON(w, http.StatusOK, ActionResponse{OK: true, NewID: rec.NewID, RootID: rec.RootID})
	}
}

// writeCreation answers with a creation's current status. A ready status is
// first checked against the store: a mismatch becomes a terminal failed
// status (uncertain false, no ids), and a failure to read identities answers
// 503 with the record unchanged.
func (s *Server) writeCreation(w http.ResponseWriter, l *Lease, status int, v creationView) {
	st := statusOf(v, s.cfg.Now())
	if st.Phase == CreatePhaseReady {
		if code := l.AuthEnded(); code != "" {
			writeErr(w, http.StatusUnauthorized, code, "access ended; the creation is unaffected — poll its status with a fresh token")
			return
		}
		verdict, err := s.verifyCreated(l.Context(), v.rec)
		if code := l.AuthEnded(); code != "" {
			writeErr(w, http.StatusUnauthorized, code, "access ended; the creation is unaffected — poll its status with a fresh token")
			return
		}
		if err != nil {
			s.cfg.Logf("direct: verify created session: %v", err)
			identityReadErr(w, err)
			return
		}
		switch verdict {
		case identityTargetUnavailable:
			st = identityFailed(st, CodeCreatedTargetUnavailable, targetUnavailableMsg)
		case identityUnverifiable:
			st = identityFailed(st, CodeCreatedIdentityUnverifiable, unverifiableMsg)
		}
	}
	s.writeStatus(w, l, status, st)
}

func identityFailed(st CreationStatus, code, msg string) CreationStatus {
	st.Phase, st.Code, st.Error, st.Uncertain = CreatePhaseFailed, code, msg, false
	st.NewID, st.RootID = "", ""
	return st
}

// rootIdentity is add-agent's identity precheck: the target must be an
// existing root with an incarnation, which the member is then pinned to.
func (s *Server) rootIdentity(l *Lease, root string) (string, error) {
	if err := s.cfg.Backend.CreationIdentity(l.Context()); err != nil {
		return "", err
	}
	cur, err := s.cfg.Backend.SessionIdentities(l.Context(), []string{root})
	if err != nil {
		return "", err
	}
	r, ok := cur[root]
	if !ok || r.RootID != "" || r.Incarnation == "" {
		return "", errNotFound
	}
	return r.Incarnation, nil
}

// applyCreation dispatches one admitted creation with its pinned root.
func (s *Server) applyCreation(ctx context.Context, act core.Action, adm admission) (amuxstore.Identity, error) {
	return s.cfg.Backend.ApplyIdentity(ctx, act, adm.rootInc)
}
