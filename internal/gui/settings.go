// settings.go 實作設定面板的 GUI 與邏輯。
//
// 設定面板以獨立子視窗呈現，由右下角齒輪按鈕觸發。
// 包含連線設定欄位（ADB Port、Proxy Port、Direct Port、STUN Server、
// TURN Server/帳號/密碼）、語言切換、以及手動檢查更新按鈕。
// TURN 帳號與密碼僅在 TURN URL 有填入值時才顯示。
//
// 設定值以 TOML 格式持久化（見 config.go），各分頁共用同一份設定。
//
// 更新相關邏輯見 settings_update.go，通用下拉選單元件見 dropdown.go。
//
// 相關文件：.claude/CLAUDE.md「專案概述 — GUI」
package gui

import (
	"fmt"
	"image"
	"image/color"
	"log/slog"
	"strconv"
	"sync"

	"gioui.org/app"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
)

// stunPreset 定義 STUN 伺服器預設選項。
type stunPreset struct {
	label string // 顯示名稱（host:port）
	value string // 完整 STUN URL（stun:host:port）
}

// defaultStunPresets 是內建的公共 STUN 伺服器清單。
// 下拉選單最後一個選項「自訂」允許使用者輸入自訂位址。
var defaultStunPresets = []stunPreset{
	{"stun.l.google.com:19302", "stun:stun.l.google.com:19302"},
	{"stun1.l.google.com:19302", "stun:stun1.l.google.com:19302"},
	{"stun2.l.google.com:19302", "stun:stun2.l.google.com:19302"},
	{"stun.cloudflare.com:3478", "stun:stun.cloudflare.com:3478"},
	{"stun.nextcloud.com:443", "stun:stun.nextcloud.com:443"},
}

// settingsPanel 是設定面板的完整狀態。
// 設定以獨立子視窗方式呈現，由右下角齒輪按鈕觸發。
// 包含設定欄位的編輯框、儲存/關閉按鈕、以及檢查更新的相關狀態。
type settingsPanel struct {
	window      *app.Window // 主視窗（用於橫幅 Invalidate）
	settingsWin *app.Window // 設定子視窗（nil 表示未開啟）

	// UI 元件 — Port 編輯框
	adbPortEditor    widget.Editor
	proxyPortEditor  widget.Editor
	directPortEditor widget.Editor
	saveBtn          widget.Clickable
	closeBtn         widget.Clickable
	checkUpdateBtn   widget.Clickable
	doUpdateBtn      widget.Clickable

	// STUN 下拉選單
	stunDrop     dropdownState    // 下拉選單 UI 狀態
	stunSelected int              // 0~len(presets)-1=preset, len(presets)=自訂
	stunEditor   widget.Editor    // 自訂 STUN 輸入框

	// TURN 模式下拉選單
	turnDrop     dropdownState    // 下拉選單 UI 狀態
	turnSelected int              // 0=Cloudflare, 1=停用, 2=自訂

	// TURN 自訂模式輸入框（自訂被選中時才顯示）
	turnEditor     widget.Editor // TURN URL 輸入框
	turnUserEditor widget.Editor // TURN 帳號輸入框
	turnPassEditor widget.Editor // TURN 密碼輸入框

	// 語言下拉選單
	langDrop dropdownState // 下拉選單 UI 狀態

	// 設定與路徑
	config     *AppConfig
	configPath string

	// 面板開關
	visible bool

	// 檢查更新狀態
	mu            sync.Mutex
	updateStatus  string // 更新狀態訊息
	updateChecked bool   // 是否已檢查過
	hasUpdate     bool   // 是否有可用更新
	checking      bool   // 正在檢查中
	updating      bool   // 正在更新中
	latestVersion string // 最新版本號

	// 更新通知橫幅（主畫面底部，非設定面板內）
	bannerDismissed  bool             // 使用者已關閉橫幅
	bannerUpdateBtn  widget.Clickable // 橫幅「立即更新」按鈕
	bannerDismissBtn widget.Clickable // 橫幅「稍後再說」按鈕

	// 捲動
	list widget.List
}

// newSettingsPanel 建立設定面板，從指定路徑載入設定。
// 若設定檔不存在，使用預設值。
func newSettingsPanel(w *app.Window) *settingsPanel {
	configPath := DefaultConfigPath()

	cfg, err := LoadConfig(configPath)
	if err != nil {
		slog.Warn("failed to load config, using defaults", "error", err)
		cfg = DefaultConfig()
	}

	p := &settingsPanel{
		window:     w,
		config:     cfg,
		configPath: configPath,
	}

	// 初始化 Port 編輯框
	p.adbPortEditor.SingleLine = true
	p.proxyPortEditor.SingleLine = true
	p.directPortEditor.SingleLine = true

	// 初始化 STUN 下拉選單
	p.stunDrop.optBtns = make([]widget.Clickable, len(defaultStunPresets)+1)
	p.stunEditor.SingleLine = true

	// 初始化 TURN 編輯框
	p.turnEditor.SingleLine = true
	p.turnUserEditor.SingleLine = true
	p.turnPassEditor.SingleLine = true
	p.turnPassEditor.Mask = '●' // 遮蔽密碼

	// 載入設定值到編輯框與下拉選單
	p.syncEditorsFromConfig()

	p.list.Axis = layout.Vertical
	return p
}

// syncEditorsFromConfig 將 config 的值同步到編輯框與 STUN 下拉選單。
func (p *settingsPanel) syncEditorsFromConfig() {
	p.adbPortEditor.SetText(strconv.Itoa(p.config.ADBPort))
	p.proxyPortEditor.SetText(strconv.Itoa(p.config.ProxyPort))
	p.directPortEditor.SetText(strconv.Itoa(p.config.DirectPort))

	// 比對 STUN 設定值是否為預設選項
	p.stunSelected = len(defaultStunPresets) // 預設選「自訂」
	for i, preset := range defaultStunPresets {
		if p.config.STUNServer == preset.value {
			p.stunSelected = i
			break
		}
	}
	// 若為自訂，填入目前設定值
	if p.stunSelected >= len(defaultStunPresets) {
		p.stunEditor.SetText(p.config.STUNServer)
	}

	// TURN 模式下拉選單（0=Cloudflare, 1=停用, 2=自訂）
	switch p.config.TURNMode {
	case TURNModeCloudflare:
		p.turnSelected = 0
	case TURNModeCustom:
		p.turnSelected = 2
	default: // "none" 或空字串 → 停用
		p.turnSelected = 1
	}

	// TURN 自訂模式輸入框
	p.turnEditor.SetText(p.config.TURNServer)
	p.turnUserEditor.SetText(p.config.TURNUser)
	p.turnPassEditor.SetText(p.config.TURNPass)
}

// openWindow 開啟獨立設定子視窗。
// 若已開啟則將既有視窗提到前景；否則建立新視窗並啟動事件迴圈 goroutine。
//
// 注意：Window.Perform 是阻塞呼叫（透過 Window.Run → eventLoop.Run → <-done），
// 若在 FrameEvent handler 中同步呼叫，會與等待 Frame 回應的原生事件迴圈形成
// 互相等待的死鎖（macOS 上因分頁合併特別容易觸發）。因此 ActionRaise 必須
// 在獨立 goroutine 中執行，並加 recover 防護視窗已銷毀時的 nil dereference。
func (p *settingsPanel) openWindow() {
	p.mu.Lock()
	if p.settingsWin != nil {
		w := p.settingsWin
		p.mu.Unlock()
		// 在獨立 goroutine 中提升視窗，避免阻塞主視窗事件迴圈導致死鎖。
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("failed to raise settings window", "error", r)
					// 視窗可能已銷毀，清除參考以便下次點擊重新建立
					p.mu.Lock()
					if p.settingsWin == w {
						p.settingsWin = nil
						p.visible = false
					}
					p.mu.Unlock()
				}
			}()
			w.Perform(system.ActionRaise)
		}()
		return
	}
	p.mu.Unlock()

	p.syncEditorsFromConfig()
	p.stunDrop.expanded = false
	p.turnDrop.expanded = false
	p.langDrop.expanded = false
	p.visible = true

	w := new(app.Window)
	w.Option(app.Title(msg().Settings.Title))
	w.Option(app.Size(unit.Dp(440), unit.Dp(200))) // 初始小尺寸，首幀自動成長至內容高度

	p.mu.Lock()
	p.settingsWin = w
	p.mu.Unlock()

	go p.settingsEventLoop(w)
}

// settingsEventLoop 是設定子視窗的事件迴圈。
// 處理畫面繪製與按鈕互動，視窗關閉時清除狀態。
// 視窗高度會根據內容自動調整（僅成長不縮小，避免閃爍）。
func (p *settingsPanel) settingsEventLoop(w *app.Window) {
	th := newThemeWithCJK()
	var ops op.Ops
	var lastH int  // 上次設定的視窗高度（px），僅成長避免閃爍
	var raised bool // 首幀後是否已設為 topmost

	defer func() {
		p.mu.Lock()
		// 只在 settingsWin 仍為自身時才清除，避免 recover 或重新建立
		// 後的新視窗參考被誤清。
		if p.settingsWin == w {
			p.settingsWin = nil
			p.visible = false
		}
		p.mu.Unlock()
		p.window.Invalidate() // 通知主視窗重繪（橫幅可能需更新）
	}()

	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)

			// 處理按鈕事件
			for p.saveBtn.Clicked(gtx) {
				p.save()
				w.Perform(system.ActionClose)
			}
			for p.closeBtn.Clicked(gtx) {
				w.Perform(system.ActionClose)
			}
			for p.checkUpdateBtn.Clicked(gtx) {
				p.startCheckUpdate()
			}
			for p.doUpdateBtn.Clicked(gtx) {
				p.startUpdate()
			}

			// 深色背景
			paint.FillShape(gtx.Ops, colorPanelBg,
				clip.Rect{Max: gtx.Constraints.Max}.Op())

			// 內容（含捲動，以防使用者手動縮小視窗）
			layout.UniformInset(unit.Dp(20)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return material.List(th, &p.list).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
					return p.layoutContent(gtx, th)
				})
			})

			// 自動調整視窗高度：讀取 List 回報的內容總高度，加上 padding
			if contentH := p.list.Position.Length; contentH > 0 {
				paddingPx := gtx.Dp(unit.Dp(40)) // 上下各 20dp
				totalPx := contentH + paddingPx
				if totalPx != lastH {
					lastH = totalPx
					h := unit.Dp(float32(totalPx) / gtx.Metric.PxPerDp)
					w.Option(app.Size(unit.Dp(440), h))
				}
			}

			e.Frame(&ops)

			// 首幀渲染後將設定視窗設為 topmost，防止 Gio Windows 後端的
			// pointerUpdate 在滑鼠經過主視窗時呼叫 SetFocus 搶回焦點，
			// 導致使用者無法操作設定視窗。
			if !raised {
				raised = true
				go func() {
					defer func() { recover() }()
					w.Perform(system.ActionRaise)
				}()
			}
		}
	}
}

// save 將編輯框的值寫入 config 並持久化到檔案。
// 回傳是否儲存成功。
func (p *settingsPanel) save() bool {
	p.config.ADBPort = parsePort(p.adbPortEditor.Text(), 5037)
	p.config.ProxyPort = parsePort(p.proxyPortEditor.Text(), 5555)
	p.config.DirectPort = parsePort(p.directPortEditor.Text(), 15555)

	// 根據下拉選單選取結果決定 STUN 值
	if p.stunSelected < len(defaultStunPresets) {
		p.config.STUNServer = defaultStunPresets[p.stunSelected].value
	} else {
		p.config.STUNServer = p.stunEditor.Text()
	}

	// TURN 模式與自訂設定（0=Cloudflare, 1=停用, 2=自訂）
	switch p.turnSelected {
	case 0:
		p.config.TURNMode = TURNModeCloudflare
	case 2:
		p.config.TURNMode = TURNModeCustom
	default:
		p.config.TURNMode = TURNModeNone
	}
	p.config.TURNServer = p.turnEditor.Text()
	p.config.TURNUser = p.turnUserEditor.Text()
	p.config.TURNPass = p.turnPassEditor.Text()

	if p.configPath == "" {
		slog.Warn("config path is empty, cannot save")
		return false
	}

	if err := SaveConfig(p.config, p.configPath); err != nil {
		slog.Error("failed to save config", "error", err)
		return false
	}

	slog.Info("config saved", "path", p.configPath)
	return true
}

// layoutContent 繪製設定面板的內部內容。
func (p *settingsPanel) layoutContent(gtx layout.Context, th *material.Theme) layout.Dimensions {
	p.mu.Lock()
	updateStatus := p.updateStatus
	hasUpdate := p.hasUpdate
	checking := p.checking
	updating := p.updating
	latestVersion := p.latestVersion
	p.mu.Unlock()

	spacing := layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// 標題
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			title := material.H6(th, msg().Settings.Title)
			title.TextSize = unit.Sp(18)
			return title.Layout(gtx)
		}),

		spacing,

		// --- 連線設定區塊 ---
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, msg().Settings.ConnectionSection)
			lbl.Color = colorPanelHint
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),

		// ADB Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "ADB Port", &p.adbPortEditor, "5037")
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// Proxy Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Proxy Port", &p.proxyPortEditor, "5555")
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// Direct Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Direct Port", &p.directPortEditor, "15555")
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// STUN Server（下拉選單）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return p.layoutStunDropdown(gtx, th)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// TURN Server（URL 有值時才顯示帳號/密碼）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return p.layoutTurnFields(gtx, th)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// 語言（下拉選單）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return p.layoutLangDropdown(gtx, th)
		}),

		spacing,

		// 儲存按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(th, &p.saveBtn, msg().Settings.SaveBtn)
			btn.Background = colorTabActive
			return btn.Layout(gtx)
		}),

		spacing,

		// --- 分隔線 ---
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(1)))
			paint.FillShape(gtx.Ops, colorPanelDivider, clip.Rect{Max: size}.Op())
			return layout.Dimensions{Size: size}
		}),

		spacing,

		// --- 版本資訊與更新 ---
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, msg().Settings.AboutSection)
			lbl.Color = colorPanelHint
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),

		// 目前版本
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			ver := fmt.Sprintf(msg().Settings.CurrentVerFmt, buildinfo.Version)
			return material.Body1(th, ver).Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),

		// 最新版本（檢查後才顯示）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if latestVersion == "" {
				return layout.Dimensions{}
			}
			ver := fmt.Sprintf(msg().Settings.LatestVerFmt, latestVersion)
			return material.Body1(th, ver).Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),

		// 更新狀態訊息
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if updateStatus == "" {
				return layout.Dimensions{}
			}
			c := colorPanelHint
			if hasUpdate {
				c = color.NRGBA{R: 255, G: 152, B: 0, A: 255} // 橘色提示（暗色面板上更亮）
			}
			return statusText(gtx, th, updateStatus, c)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// 檢查更新 / 執行更新按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if hasUpdate && !updating {
				// 有更新可用：顯示「立即更新」按鈕
				btn := material.Button(th, &p.doUpdateBtn, msg().Settings.UpdateNow)
				btn.Background = color.NRGBA{R: 230, G: 126, B: 34, A: 255}
				return btn.Layout(gtx)
			}
			// 一般狀態：顯示「檢查更新」按鈕
			label := msg().Settings.CheckUpdate
			if checking {
				label = msg().Settings.Checking
			}
			if updating {
				label = msg().Settings.Updating
			}
			btn := material.Button(th, &p.checkUpdateBtn, label)
			btn.Background = colorModeActive
			return btn.Layout(gtx)
		}),

		spacing,

		// 關閉按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(th, &p.closeBtn, msg().Settings.CloseBtn)
			btn.Background = colorTabInactive
			return btn.Layout(gtx)
		}),
	)
}

// layoutStunDropdown 繪製 STUN 伺服器下拉選單。
// 包含預設公共 STUN 伺服器清單，最後一個選項「自訂」允許使用者輸入自訂位址。
func (p *settingsPanel) layoutStunDropdown(gtx layout.Context, th *material.Theme) layout.Dimensions {
	totalOpts := len(defaultStunPresets) + 1

	// 組裝選項清單
	opts := make([]dropdownOption, totalOpts)
	for i := range defaultStunPresets {
		opts[i] = dropdownOption{Label: defaultStunPresets[i].label, Selected: i == p.stunSelected}
	}
	opts[totalOpts-1] = dropdownOption{Label: msg().Settings.CustomStunOption, Selected: p.stunSelected >= len(defaultStunPresets)}

	// 目前選取的顯示文字
	var currentLabel string
	if p.stunSelected < len(defaultStunPresets) {
		currentLabel = defaultStunPresets[p.stunSelected].label
	} else {
		currentLabel = msg().Settings.CustomStun
	}

	// 自訂選項被選中時：收合狀態下附加自訂輸入框
	var extra []layout.FlexChild
	if p.stunSelected >= len(defaultStunPresets) {
		extra = []layout.FlexChild{
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "", &p.stunEditor, "stun:your.server.com:3478")
			}),
		}
	}

	return p.stunDrop.layoutDropdown(gtx, th, "STUN Server", currentLabel, opts, func(idx int) {
		p.stunSelected = idx
	}, extra...)
}

// layoutTurnFields 繪製 TURN 伺服器下拉選單與自訂輸入框。
// 下拉選單提供三個選項：Cloudflare（免費）、停用、自訂。
// 選擇自訂時顯示 URL、帳號、密碼輸入框。
func (p *settingsPanel) layoutTurnFields(gtx layout.Context, th *material.Theme) layout.Dimensions {
	opts := []dropdownOption{
		{Label: msg().Settings.TURNModeCloudflare, Selected: p.turnSelected == 0},
		{Label: msg().Settings.TURNModeNone, Selected: p.turnSelected == 1},
		{Label: msg().Settings.TURNModeCustom, Selected: p.turnSelected == 2},
	}

	currentLabel := opts[p.turnSelected].Label

	// 自訂模式被選中時（索引 2）：收合狀態下附加 URL/帳號/密碼輸入框
	var extra []layout.FlexChild
	if p.turnSelected == 2 {
		extra = []layout.FlexChild{
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, msg().Settings.TURNLabel, &p.turnEditor, msg().Settings.TURNHint)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, msg().Settings.TURNUserLabel, &p.turnUserEditor, "radb")
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, msg().Settings.TURNPassLabel, &p.turnPassEditor, "")
			}),
		}
	}

	return p.turnDrop.layoutDropdown(gtx, th, msg().Settings.TURNModeLabel, currentLabel, opts, func(idx int) {
		p.turnSelected = idx
	}, extra...)
}

// layoutLangDropdown 繪製語言下拉選單。
// 提供三個選項：自動偵測、繁體中文、English。
// 切換語言後即時生效（更新全域 Messages 並刷新視窗標題）。
func (p *settingsPanel) layoutLangDropdown(gtx layout.Context, th *material.Theme) layout.Dimensions {
	langCodes := []string{LangAuto, LangZhTW, LangEN}
	langLabels := []string{msg().Settings.LanguageAuto, "繁體中文", "English"}

	// 找出目前選取的索引
	currentIdx := 0
	for i, code := range langCodes {
		if code == p.config.Language {
			currentIdx = i
			break
		}
	}

	opts := make([]dropdownOption, len(langLabels))
	for i, label := range langLabels {
		opts[i] = dropdownOption{Label: label, Selected: i == currentIdx}
	}

	return p.langDrop.layoutDropdown(gtx, th, msg().Settings.LanguageLabel, langLabels[currentIdx], opts, func(idx int) {
		p.config.Language = langCodes[idx]
		SetLanguage(p.config.Language)
		// 刷新主視窗標題（非阻塞，避免在 FrameEvent 中呼叫 Option 死鎖）
		go p.window.Option(app.Title(guiWindowTitle()))
		// 刷新設定視窗標題
		p.mu.Lock()
		if p.settingsWin != nil {
			go p.settingsWin.Option(app.Title(msg().Settings.Title))
		}
		p.mu.Unlock()
		p.window.Invalidate()
	})
}

// invalidateAll 通知主視窗和設定子視窗重繪。
// 用於背景 goroutine 更新狀態後觸發 UI 刷新。
func (p *settingsPanel) invalidateAll() {
	p.window.Invalidate()
	p.mu.Lock()
	w := p.settingsWin
	p.mu.Unlock()
	if w != nil {
		w.Invalidate()
	}
}
