// Package sessionrpc implements authenticated one-shot RPC over bounded regular
// files for restricted amux sessions. It deliberately contains no daemon policy.
package sessionrpc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"amux/internal/access"
)

const (
	ProtocolVersion = access.ProtocolVersion

	ContextFileName   = access.ContextFileName
	CredentialFile    = "current"
	ServiceFileName   = "service.json"
	RequestsDirName   = "requests"
	ResponsesDirName  = "responses"
	processingDirName = "processing"

	MaxQueuedRequests   = access.MaxQueuedRequests
	MaxResponseBody     = access.MaxBodyBytes
	MaxReceiptGrace     = 30 * time.Second
	DefaultReceiptGrace = 3 * time.Second
	DefaultPollInterval = 20 * time.Millisecond
	MaxPendingReceipts  = MaxQueuedRequests
	MaxResponseFiles    = MaxQueuedRequests
	// Request temporaries older than every valid request lifetime are abandoned.
	// A scanner must preserve newer temporaries because the publishing client
	// may still hold and fsync the file before its no-replace rename.
	requestTemporaryMaxAge = 2 * access.MaxRequestAge

	maxContextFileBytes    = 8 << 10
	maxCredentialFileBytes = 64 << 10
	maxServiceFileBytes    = 8 << 10
	// SignedRequest/Response bodies are base64 JSON strings. Bound the encoded
	// file independently while retaining access.MaxBodyBytes exact body bytes.
	maxEnvelopeFileBytes = ((access.MaxBodyBytes+2)/3)*4 + (8 << 10)
	// MaxResponseBytes is the per-subject bound for daemon-created response
	// files. Count admission remains independently enforced.
	MaxResponseBytes = int64(8 << 20)
)

var (
	ErrInvalidRecord    = errors.New("invalid session RPC record")
	ErrInvalidSignature = errors.New("invalid session RPC signature")
	ErrQueueFull        = errors.New("session RPC queue capacity exceeded")
	ErrResponseCapacity = errors.New("session RPC response capacity exceeded")
	ErrRestarted        = errors.New("session RPC daemon boot changed")
	ErrIndeterminate    = errors.New("session RPC outcome is indeterminate")
	ErrClosed           = errors.New("session RPC endpoint is closed")
)

type CallKind string

const (
	CallOperation CallKind = "call"
	CallReceipt   CallKind = "receipt"
)

type Status string

const (
	StatusOK            Status = "ok"
	StatusDenied        Status = "denied"
	StatusInvalid       Status = "invalid"
	StatusFailed        Status = "failed"
	StatusReplay        Status = "replay"
	StatusIndeterminate Status = "indeterminate"
)

// SessionContext is intentionally static: mutable boot and credential state do
// not belong here. The trusted launcher publishes it at the fixed access root.
type SessionContext = access.SessionContext

type Service struct {
	Protocol    int    `json:"protocol"`
	BootID      string `json:"bootId"`
	SubjectID   string `json:"subjectId"`
	IssuerKeyID string `json:"issuerKeyId"`
	PublishedAt int64  `json:"publishedAt"`
	Signature   string `json:"signature"`
}

type Receipt struct {
	RequestID      string `json:"requestId"`
	ResponseDigest string `json:"responseDigest"`
}

// Call is decoded only after access.Authority.Verify durably accepts its exact
// encoded bytes. Route/verb/resource fields are inputs to daemon-owned policy,
// never grants in themselves.
type Call struct {
	Kind    CallKind          `json:"kind"`
	Route   access.Route      `json:"route,omitempty"`
	Verb    string            `json:"verb,omitempty"`
	ID      string            `json:"id,omitempty"`
	Target  string            `json:"target,omitempty"`
	Tab     int               `json:"tab,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"`
	Receipt *Receipt          `json:"receipt,omitempty"`
}

func (c Call) AccessRequest() access.Request {
	fields := make(map[string]string, len(c.Fields))
	for key, value := range c.Fields {
		fields[key] = value
	}
	return access.Request{Route: c.Route, Verb: c.Verb, ID: c.ID, Target: c.Target, Tab: c.Tab, Fields: fields}
}

func cloneCall(call Call) Call {
	clone := call
	if call.Fields != nil {
		clone.Fields = make(map[string]string, len(call.Fields))
		for key, value := range call.Fields {
			clone.Fields[key] = value
		}
	}
	if call.Receipt != nil {
		receipt := *call.Receipt
		clone.Receipt = &receipt
	}
	return clone
}

type Response struct {
	Protocol          int    `json:"protocol"`
	BootID            string `json:"bootId"`
	IssuerKeyID       string `json:"issuerKeyId"`
	SubjectID         string `json:"subjectId"`
	KeyID             string `json:"keyId"`
	Generation        uint64 `json:"generation"`
	RequestID         string `json:"requestId"`
	RequestBodyDigest string `json:"requestBodyDigest"`
	Status            Status `json:"status"`
	Code              string `json:"code,omitempty"`
	Body              []byte `json:"body,omitempty"`
	CompletedAt       int64  `json:"completedAt"`
	ReceiptRequired   bool   `json:"receiptRequired,omitempty"`
	Signature         string `json:"signature"`
}

type Result struct {
	Status      Status
	Code        string
	Body        []byte
	RequestID   string
	ReceiptSent bool
}

type RemoteError struct {
	Status Status
	Code   string
}

func (e *RemoteError) Error() string {
	if e.Code == "" {
		return "session RPC failed: " + string(e.Status)
	}
	return fmt.Sprintf("session RPC failed: %s (%s)", e.Status, e.Code)
}

type Query struct {
	Verb   string
	ID     string
	Target string
	Tab    int
	Fields map[string]string
}

func (q Query) call() Call {
	return Call{Kind: CallOperation, Route: access.RouteQuery, Verb: q.Verb, ID: q.ID,
		Target: q.Target, Tab: q.Tab, Fields: q.Fields}
}

type Action struct {
	Verb   string
	ID     string
	Target string
	Tab    int
	Fields map[string]string
}

func (a Action) call() Call {
	return Call{Kind: CallOperation, Route: access.RouteAction, Verb: a.Verb, ID: a.ID,
		Target: a.Target, Tab: a.Tab, Fields: a.Fields}
}

type Authority interface {
	BootID() string
	Verify(context.Context, access.SignedRequest) (access.Principal, error)
	Valid(context.Context, access.Principal) error
}

// IssuerSigner is expected to be implemented by access authority without
// revealing its issuer private key to this package or daemon integration.
type IssuerSigner interface {
	IssuerKeyID() string
	SignIssuer([]byte) ([]byte, error)
}

type DispatchRequest struct {
	Principal access.Principal
	RequestID string
	Call      Call
}

type PersistedResponse struct {
	RequestID      string
	ResponseDigest string
	CompletedAt    time.Time
}

type ReceiptSettlement struct {
	RequestID string
	Received  bool
	Reason    SettlementReason
	At        time.Time
}

type SettlementReason string

const (
	SettlementReceipt        SettlementReason = "receipt"
	SettlementGraceExpired   SettlementReason = "grace_expired"
	SettlementResponseFailed SettlementReason = "response_failed"
	SettlementCapacity       SettlementReason = "capacity"
	SettlementServerClosed   SettlementReason = "server_closed"
)

type ReceiptHooks struct {
	Grace time.Duration
	// ResponsePersisted and Settled are synchronous ordering boundaries. Trusted
	// daemon integration must make them bounded, nonblocking, and non-panicking;
	// the transport does not detach lifecycle completion from these callbacks.
	ResponsePersisted func(PersistedResponse)
	// Settled is required when ReceiptHooks is returned. It runs exactly once,
	// including when signing or durable response publication fails after the
	// operation committed. ResponsePersisted is never called on those failures.
	Settled func(ReceiptSettlement)
}

type DispatchResult struct {
	Status  Status
	Code    string
	Body    []byte
	Receipt *ReceiptHooks
}

// ResponseBudget atomically admits daemon-wide response amplification across
// independently served subjects. A daemon implementation must include both
// file count and MaxBytes in one reservation decision. Each returned lease is
// committed after durable publication and released only after the corresponding
// response file is removed. ReserveResponse must serialize admission across
// subjects and idempotently return the existing lease when Existing is true.
// Existing adopts disk reality and must succeed even when retained files exceed
// the current admission limit; new amplification stays blocked until cleanup
// releases enough leases. All lease methods must be idempotent.
type ResponseBudget interface {
	ReserveResponse(context.Context, ResponseReservation) (ResponseLease, error)
}

type ResponseReservation struct {
	SubjectID   string
	RequestID   string
	MaxBytes    int64
	Existing    bool
	ReceiptSlot bool
}

type ResponseLease interface {
	Commit(actualBytes int64, receiptPending bool)
	ReleaseReceipt()
	Release()
}

type Callbacks struct {
	// All callbacks are trusted synchronous boundaries. Implementations must
	// honor context cancellation, remain bounded and nonblocking, and not panic.
	// The package serializes calls per subject but does not isolate daemon code.
	Authorize func(context.Context, access.Principal, Call) error
	// Dispatch is the execution boundary. Integration must compare the
	// authenticated Principal.Generation with current authority and policy under
	// the same lock or transaction that admits the operation's effects.
	Dispatch func(context.Context, DispatchRequest) (DispatchResult, error)
}

func validateCall(call Call) error {
	switch call.Kind {
	case CallOperation:
		if call.Receipt != nil || (call.Route != access.RouteQuery && call.Route != access.RouteAction) || call.Verb == "" {
			return ErrInvalidRecord
		}
	case CallReceipt:
		if call.Receipt == nil || !validRequestID(call.Receipt.RequestID) || !validDigest(call.Receipt.ResponseDigest) ||
			call.Route != "" || call.Verb != "" || call.ID != "" || call.Target != "" || call.Tab != 0 || len(call.Fields) != 0 {
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	return nil
}

func serviceSigningBytes(service Service) []byte {
	return framed("amux-session-rpc-service-v1", strconv.Itoa(service.Protocol), service.BootID,
		service.SubjectID, service.IssuerKeyID, strconv.FormatInt(service.PublishedAt, 10))
}

func responseSigningBytes(response Response) []byte {
	bodyDigest := sha256.Sum256(response.Body)
	return framed("amux-session-rpc-response-v1", strconv.Itoa(response.Protocol), response.BootID,
		response.IssuerKeyID, response.SubjectID, response.KeyID, strconv.FormatUint(response.Generation, 10),
		response.RequestID, response.RequestBodyDigest, string(response.Status), response.Code,
		hex.EncodeToString(bodyDigest[:]), strconv.FormatInt(response.CompletedAt, 10),
		strconv.FormatBool(response.ReceiptRequired))
}

func responseDigest(response Response) string {
	digest := sha256.Sum256(append(responseSigningBytes(response), []byte(response.Signature)...))
	return hex.EncodeToString(digest[:])
}

func signService(signer IssuerSigner, service *Service) error {
	if signer == nil || service.IssuerKeyID == "" || service.IssuerKeyID != signer.IssuerKeyID() {
		return ErrInvalidRecord
	}
	sig, err := signer.SignIssuer(serviceSigningBytes(*service))
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize {
		return ErrInvalidSignature
	}
	service.Signature = base64.RawStdEncoding.EncodeToString(sig)
	return nil
}

func signResponse(signer IssuerSigner, response *Response) error {
	if signer == nil || response.IssuerKeyID == "" || response.IssuerKeyID != signer.IssuerKeyID() {
		return ErrInvalidRecord
	}
	sig, err := signer.SignIssuer(responseSigningBytes(*response))
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize {
		return ErrInvalidSignature
	}
	response.Signature = base64.RawStdEncoding.EncodeToString(sig)
	return nil
}

func verifyService(credential access.Credential, service Service) error {
	if service.Protocol != ProtocolVersion || service.IssuerKeyID != credential.IssuerKeyID ||
		!validBootID(service.BootID) || service.SubjectID == "" || service.PublishedAt <= 0 {
		return ErrInvalidRecord
	}
	return verifyIssuer(credential, serviceSigningBytes(service), service.Signature)
}

func verifyResponse(credential access.Credential, response Response) error {
	if response.Protocol != ProtocolVersion || response.IssuerKeyID != credential.IssuerKeyID ||
		!validBootID(response.BootID) || response.SubjectID == "" || !validRequestID(response.KeyID) ||
		!validRequestID(response.RequestID) || !validDigest(response.RequestBodyDigest) ||
		!validStatus(response.Status) || !validCode(response.Code) || response.CompletedAt <= 0 || len(response.Body) > MaxResponseBody {
		return ErrInvalidRecord
	}
	return verifyIssuer(credential, responseSigningBytes(response), response.Signature)
}

func verifyIssuer(credential access.Credential, message []byte, encodedSignature string) error {
	publicKey, err := base64.RawStdEncoding.DecodeString(credential.IssuerPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return ErrInvalidSignature
	}
	signature, err := base64.RawStdEncoding.DecodeString(encodedSignature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), message, signature) {
		return ErrInvalidSignature
	}
	return nil
}

func validStatus(status Status) bool {
	switch status {
	case StatusOK, StatusDenied, StatusInvalid, StatusFailed, StatusReplay, StatusIndeterminate:
		return true
	default:
		return false
	}
}

func validCode(code string) bool {
	if len(code) > 64 {
		return false
	}
	for _, char := range code {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' && char != '.' {
			return false
		}
	}
	return true
}

func validRequestID(id string) bool { return validLowerHex(id, 16) }
func validBootID(id string) bool    { return validLowerHex(id, 32) }
func validDigest(value string) bool { return validLowerHex(value, sha256.Size) }

func validEnvelopeShape(request access.SignedRequest) bool {
	return request.Protocol == ProtocolVersion && validBootID(request.BootID) &&
		validRequestID(request.KeyID) && validRequestID(request.RequestID) &&
		request.Generation != 0 && request.IssuedAt > 0 && request.ExpiresAt > 0 &&
		len(request.Body) > 0 && len(request.Body) <= access.MaxBodyBytes && len(request.Signature) == 86
}

func validLowerHex(value string, size int) bool {
	if len(value) != size*2 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func framed(parts ...string) []byte {
	var out []byte
	for _, part := range parts {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		out = append(out, size[:]...)
		out = append(out, part...)
	}
	return out
}

func marshalBounded(value any, max int) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 || len(encoded) > max {
		return nil, ErrInvalidRecord
	}
	return encoded, nil
}

func unmarshalBounded(data []byte, max int, out any) error {
	if len(data) == 0 || len(data) > max {
		return ErrInvalidRecord
	}
	target := reflect.TypeOf(out)
	if target == nil || target.Kind() != reflect.Pointer || target.Elem().Kind() != reflect.Struct {
		return ErrInvalidRecord
	}
	if err := rejectNoncanonicalJSON(data, target.Elem()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	return nil
}

func rejectNoncanonicalJSON(data []byte, target reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeStrictJSONValue(decoder, target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func consumeStrictJSONValue(decoder *json.Decoder, target reflect.Type) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		fields, mapValue, arbitrary := canonicalObjectFields(target)
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("non-string JSON object key")
			}
			if _, duplicate := keys[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			keys[key] = struct{}{}
			fieldTarget := mapValue
			if !arbitrary {
				var found bool
				fieldTarget, found = fields[key]
				if !found {
					return fmt.Errorf("noncanonical JSON object key %q", key)
				}
			}
			if err := consumeStrictJSONValue(decoder, fieldTarget); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		element := canonicalElementType(target)
		for decoder.More() {
			if err := consumeStrictJSONValue(decoder, element); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected closing JSON delimiter")
	}
	return nil
}

// canonicalObjectFields returns the exact JSON spellings accepted for a wire
// struct. Maps intentionally retain arbitrary string keys (for Call.Fields),
// while their values are still recursively checked for duplicate structure.
func canonicalObjectFields(target reflect.Type) (map[string]reflect.Type, reflect.Type, bool) {
	target = indirectType(target)
	if target == nil || target.Kind() == reflect.Interface {
		return nil, nil, true
	}
	if target.Kind() == reflect.Map && target.Key().Kind() == reflect.String {
		return nil, target.Elem(), true
	}
	fields := make(map[string]reflect.Type)
	if target.Kind() != reflect.Struct {
		return fields, nil, false
	}
	for index := 0; index < target.NumField(); index++ {
		field := target.Field(index)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields, nil, false
}

func canonicalElementType(target reflect.Type) reflect.Type {
	target = indirectType(target)
	if target == nil || target.Kind() == reflect.Interface {
		return nil
	}
	if target.Kind() == reflect.Array || target.Kind() == reflect.Slice {
		return target.Elem()
	}
	return nil
}

func indirectType(target reflect.Type) reflect.Type {
	for target != nil && target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	return target
}
