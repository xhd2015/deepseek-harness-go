package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/xhd2015/deepseek-harness-go/mux"
)

// APIPrefix is the path prefix the HTTP bridge mounts under.
const APIPrefix = "/api/"

// maxRPCBody bounds one HTTP RPC request.
const maxRPCBody = 64 << 20

// ClientRequest is the envelope an out-of-process client posts.
type ClientRequest struct {
	Type    string          `json:"type"`
	RPCID   string          `json:"rpcId"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

// ServerResponse is the envelope the HTTP bridge answers with.
type ServerResponse struct {
	Type   string      `json:"type"`
	RPCID  string      `json:"rpcId"`
	Result *RPCOutcome `json:"result"`
}

// RPCOutcome is one call's success value or structured failure.
type RPCOutcome struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error *RPCFailure     `json:"error,omitempty"`
}

// RPCFailure is the structured error an unary call reports.
type RPCFailure struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// codeBadRequest is the gateway's own rejection code.
const codeBadRequest = "gateway/bad-request"

// serveRPC answers `POST /api/<method>`: the same unary calls the browser makes
// over the WebSocket, so an out-of-process client never has to fake a page.
func (s *Server) serveRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		// A rejected credential is a transport-level answer, the same 401 the
		// browser channel reports; the body is not an RPC envelope.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	endpoint := r.URL.Path[len(APIPrefix):]
	if endpoint == "" || !validEndpoint(endpoint) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	mediaType := r.Header.Get("Content-Type")
	if index := strings.IndexByte(mediaType, ';'); index >= 0 {
		mediaType = mediaType[:index]
	}
	if strings.ToLower(strings.TrimSpace(mediaType)) != "application/json" {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRPCBody))
	if err != nil {
		writeRPCError(w, "", RPCFailure{Code: codeBadRequest, Message: "cannot read request body"})
		return
	}
	var envelope ClientRequest
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeRPCError(w, "", RPCFailure{Code: codeBadRequest, Message: "body is not a client request"})
		return
	}
	if envelope.Type != "client-request" {
		writeRPCError(w, envelope.RPCID, RPCFailure{Code: codeBadRequest, Message: "request type must be client-request"})
		return
	}
	if envelope.RPCID == "" {
		writeRPCError(w, "", RPCFailure{Code: codeBadRequest, Message: "request is missing rpcId"})
		return
	}
	if envelope.Method != endpoint {
		writeRPCError(w, envelope.RPCID, RPCFailure{
			Code:    codeBadRequest,
			Message: fmt.Sprintf("method %q does not match endpoint %q", envelope.Method, endpoint),
		})
		return
	}
	args, err := rpcArgs(envelope.Payload)
	if err != nil {
		writeRPCError(w, envelope.RPCID, RPCFailure{Code: codeBadRequest, Message: err.Error()})
		return
	}
	value, callErr := s.cfg.Service.Unary(endpoint, args)
	if callErr != nil {
		failure := failureOf(callErr)
		writeRPCError(w, envelope.RPCID, failure)
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		writeRPCError(w, envelope.RPCID, RPCFailure{Code: "gateway/internal", Message: err.Error()})
		return
	}
	writeRPC(w, ServerResponse{
		Type:  "server-response",
		RPCID: envelope.RPCID,
		Result: &RPCOutcome{
			OK:    true,
			Value: raw,
		},
	})
}

// validEndpoint rejects traversal and empty path segments.
func validEndpoint(endpoint string) bool {
	if len(endpoint) > 512 {
		return false
	}
	for _, segment := range strings.Split(endpoint, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, r := range segment {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '_', r == '-', r == '.':
			case r == '$':
				// The protocol reserves a `$` prefix for client-side plumbing; the
				// waterfall answer (`$events/result`) is one of those endpoints and
				// must be reachable or an approval can never be answered.
			default:
				return false
			}
		}
	}
	return true
}

// rpcArgs validates the `{"args": {...}}` payload the typed client sends.
func rpcArgs(payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) == 0 {
		return nil, errors.New("request payload is empty")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, errors.New("request payload is not an object")
	}
	if len(envelope) != 1 {
		return nil, errors.New("request payload must contain exactly one args field")
	}
	args, ok := envelope["args"]
	if !ok {
		return nil, errors.New("request payload must contain exactly one args field")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(args)), "{") {
		return nil, errors.New("request args must be an object")
	}
	return args, nil
}

// failureOf maps one service error onto the RPC failure envelope.
func failureOf(err error) RPCFailure {
	var failure *mux.Failure
	if errors.As(err, &failure) && failure != nil {
		details := failure.Details
		if details == nil {
			details = map[string]any{}
		}
		return RPCFailure{Code: failure.Code, Message: failure.Message, Details: details}
	}
	return RPCFailure{Code: "gateway/internal", Message: err.Error(), Details: map[string]any{}}
}

func writeRPCError(w http.ResponseWriter, rpcID string, failure RPCFailure) {
	if failure.Details == nil {
		failure.Details = map[string]any{}
	}
	writeRPC(w, ServerResponse{
		Type:   "server-response",
		RPCID:  rpcID,
		Result: &RPCOutcome{OK: false, Error: &failure},
	})
}

func writeRPC(w http.ResponseWriter, response ServerResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}
