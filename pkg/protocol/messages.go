package protocol

// ErrorCode represents structured error codes for protocol responses.
type ErrorCode string

const (
	ErrorCodeNone             ErrorCode = ""
	ErrorCodeInvalidToken     ErrorCode = "invalid_token"
	ErrorCodeAlreadyConnected ErrorCode = "already_connected"
	ErrorCodeNoDomains        ErrorCode = "no_domains"
)

// AuthRequest is the first message sent by the client to authenticate using a token.
type AuthRequest struct {
	Token string `json:"token"`
	Force bool   `json:"force,omitempty"` // Force disconnect existing session
}

// TunnelRequest follows authentication to request binding of specific domains.
type TunnelRequest struct {
	RequestedDomains []string `json:"requested_domains"`
}

// InitResponse is sent by the server to indicate success or failure of the handshake.
type InitResponse struct {
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
	ErrorCode ErrorCode `json:"error_code,omitempty"` // Structured error code
	// AssignedDomains could be useful if we support random assignment (future),
	// but for now it confirms what was bound.
	BoundDomains []string `json:"bound_domains,omitempty"`
}
