package protocol

// AuthRequest is the first message sent by the client to authenticate using a token.
type AuthRequest struct {
	Token string `json:"token"`
}

// TunnelRequest follows authentication to request binding of specific domains.
type TunnelRequest struct {
	RequestedDomains []string `json:"requested_domains"`
}

// InitResponse is sent by the server to indicate success or failure of the handshake.
type InitResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	// AssignedDomains could be useful if we support random assignment (future),
	// but for now it confirms what was bound.
	BoundDomains []string `json:"bound_domains,omitempty"`
}
