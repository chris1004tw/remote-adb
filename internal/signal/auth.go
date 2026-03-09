package signal

// Authenticator 驗證連線端提供的 Token。
type Authenticator interface {
	Validate(token string) bool
}

// PSKAuth 使用 Pre-Shared Key 進行驗證。
type PSKAuth struct {
	token string
}

// NewPSKAuth 建立一個 PSK 驗證器。
func NewPSKAuth(token string) *PSKAuth {
	return &PSKAuth{token: token}
}

// Validate 驗證提供的 token 是否與 PSK 相符。
func (a *PSKAuth) Validate(token string) bool {
	return token != "" && token == a.token
}
