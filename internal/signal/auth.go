// Package signal — 認證模組。
//
// 本檔案定義信令伺服器的認證機制。目前僅實作 PSK（Pre-Shared Key）認證：
// 伺服器啟動時設定一組共享密鑰，Agent 與 Client 連線時必須提供相同的密鑰才能通過認證。
//
// PSK 認證的適用場景：
//   - 內網或私有部署環境，所有參與者都可以預先取得密鑰
//   - 不需要使用者帳號系統的輕量化情境
//
// 若未來需要更複雜的認證機制（如 OAuth、JWT），只要實作 Authenticator 介面即可替換。
package signal

// Authenticator 定義認證策略的介面。
// 信令伺服器透過此介面驗證連線端提供的 Token，與具體實作解耦。
// 不同的部署環境可以提供不同的認證策略（目前僅有 PSK）。
type Authenticator interface {
	Validate(token string) bool
}

// PSKAuth 使用 Pre-Shared Key（預共享金鑰）進行驗證。
// 所有合法的 Agent 與 Client 都必須持有相同的 token 才能通過認證。
// 這是最簡單的認證方式，適合信任網路內的部署環境。
type PSKAuth struct {
	token string // 伺服器端的預共享金鑰，由環境變數或設定檔提供
}

// NewPSKAuth 建立一個 PSK 驗證器。
// token 參數為伺服器端的預共享金鑰。
func NewPSKAuth(token string) *PSKAuth {
	return &PSKAuth{token: token}
}

// Validate 驗證提供的 token 是否與 PSK 相符。
// 刻意將空字串視為無效，即使伺服器端 token 也是空字串也不會通過。
// 理由：空 token 表示「未設定認證」，允許空 token 通過等同於繞過認證，
// 這可能導致未經授權的連線在未設定密鑰的部署中被意外接受。
func (a *PSKAuth) Validate(token string) bool {
	return token != "" && token == a.token
}
