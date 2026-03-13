// cmd_p2p.go — P2P 模式子命令（radb p2p connect / radb p2p agent）。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chris1004tw/remote-adb/internal/bridge"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

// cmdConnectPair 透過 P2P SDP 配對建立全設備多工 ADB 轉發（radb p2p connect）。
// 流程：
//  1. 建立 PeerConnection + control DataChannel
//  2. 產生 Offer SDP → 壓縮編碼為邀請碼 token
//  3. 使用者手動將邀請碼傳給被控端，貼入回應碼
//  4. HandleAnswer → 等待 P2P 連線建立
//  5. 啟動 control channel 讀取迴圈（接收設備清單）
//  6. 建立 DeviceProxyManager（每台設備獨立 proxy port）
func cmdConnectPair(args []string) {
	fs := flag.NewFlagSet("p2p connect", flag.ExitOnError)
	ice := addICEFlags(fs)
	portStart := fs.Int("port", envInt("RADB_PROXY_PORT", 5555), "本機 ADB proxy port 起始值")
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server port")
	fs.Parse(args)

	// 建立 ICE config（支援 Cloudflare 免費 TURN）
	iceConfig := ice.build()

	// 建立 PeerConnection
	pm, err := webrtc.NewPeerManager(iceConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 PeerConnection 失敗: %v\n", err)
		os.Exit(1)
	}
	defer pm.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 建立 control DataChannel
	controlCh, err := pm.OpenChannel("control")
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 control channel 失敗: %v\n", err)
		os.Exit(1)
	}

	// 產生 compact SDP offer
	fmt.Fprintln(os.Stderr, "正在收集 ICE 候選...")
	offerSDP, err := pm.CreateOffer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 Offer 失敗: %v\n", err)
		os.Exit(1)
	}

	compact := bridge.SDPToCompact(offerSDP)
	token, err := bridge.EncodeToken(compact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "編碼 token 失敗: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "\n邀請碼（複製給被控端）:")
	fmt.Println(token)
	fmt.Fprintln(os.Stderr, "\n請輸入回應碼:")

	// 讀取回應碼（重試迴圈：空輸入或無效 token 時重新要求，
	// 因為邀請碼僅限使用一次，不能因誤按 Enter 就放棄）
	scanner := bufio.NewScanner(os.Stdin)
	var answerCompact bridge.CompactSDP
	for {
		var answerToken string
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				answerToken = line
				break
			}
		}
		if answerToken == "" {
			// stdin EOF（管線結束），無法重試
			fmt.Fprintln(os.Stderr, "未輸入回應碼")
			os.Exit(1)
		}
		compact, decErr := bridge.DecodeToken(answerToken)
		if decErr != nil {
			fmt.Fprintf(os.Stderr, "無效的回應碼（%v），請重新輸入:\n", decErr)
			continue
		}
		answerCompact = compact
		break
	}

	// 先註冊回呼再啟動 ICE，避免 LAN 環境下 ICE 在毫秒內完成導致回呼未觸發
	connCh := make(chan struct{})
	pm.OnConnected(func(relayed bool) {
		if relayed {
			fmt.Fprintln(os.Stderr, "注意：連線透過 TURN 中繼（延遲較高）")
		}
		close(connCh)
	})

	answerSDP := bridge.CompactToSDP(answerCompact)
	if err := pm.HandleAnswer(answerSDP); err != nil {
		fmt.Fprintf(os.Stderr, "處理回應失敗: %v\n", err)
		os.Exit(1)
	}

	// 等待連線建立
	select {
	case <-connCh:
	case <-time.After(30 * time.Second):
		fmt.Fprintln(os.Stderr, "連線逾時")
		os.Exit(1)
	case <-ctx.Done():
		return
	}

	fmt.Fprintln(os.Stderr, "P2P 連線已建立")

	// 建立 per-device proxy 管理器
	adbAddr := fmt.Sprintf("127.0.0.1:%d", *adbPort)
	onReady, onRemoved := cliDeviceProxyCallbacks(adbAddr)
	dpm := bridge.NewDeviceProxyManager(bridge.DeviceProxyConfig{
		PortStart: *portStart,
		OpenCh:    pm.OpenChannel,
		ADBAddr:   adbAddr,
		OnReady:   onReady,
		OnRemoved: onRemoved,
	})
	defer dpm.Close()

	fmt.Fprintln(os.Stderr, "按 Ctrl+C 結束")

	// 啟動 control channel 讀取（驅動 DeviceProxyManager 的設備增減）
	go func() {
		bridge.ControlReadLoop(ctx, controlCh, func(cm bridge.CtrlMessage) {
			switch cm.Type {
			case "hello":
				fmt.Fprintf(os.Stderr, "遠端主機: %s\n", cm.Hostname)
			case "devices":
				dpm.UpdateDevices(cm.Devices)
			}
		})
	}()

	<-ctx.Done()
}

// cmdAgentPair 處理一次性 P2P 被控端連線（radb p2p agent）。
// 接收主控端的邀請碼（compact SDP），建立 Answer 並等待 P2P 連線。
// 連線建立後透過 control channel 推送設備清單，並由 ServerHandler
// 處理所有 DataChannel（adb-server/adb-stream/adb-fwd）。
func cmdAgentPair(args []string) {
	fs := flag.NewFlagSet("p2p agent", flag.ExitOnError)
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
	ice := addICEFlags(fs)
	fs.Parse(args)

	// 支援兩種用法：直接帶邀請碼（radb p2p agent <token>）或互動輸入
	var offerToken string
	if fs.NArg() >= 1 {
		offerToken = fs.Arg(0)
	} else {
		fmt.Fprintln(os.Stderr, "請輸入邀請碼:")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				offerToken = line
				break
			}
		}
		if offerToken == "" {
			fmt.Fprintln(os.Stderr, "未輸入邀請碼")
			os.Exit(1)
		}
	}

	// 解碼 compact SDP offer
	offerCompact, err := bridge.DecodeToken(strings.TrimSpace(offerToken))
	if err != nil {
		fmt.Fprintf(os.Stderr, "無效的邀請碼: %v\n", err)
		os.Exit(1)
	}

	offerSDP := bridge.CompactToSDP(offerCompact)

	// 建立 ICE config（支援 Cloudflare 免費 TURN）
	iceConfig := ice.build()

	// 建立 PeerConnection
	pm, err := webrtc.NewPeerManager(iceConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 PeerConnection 失敗: %v\n", err)
		os.Exit(1)
	}
	defer pm.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 偵測 PeerConnection 斷線（主控端關閉時自動結束 agent）
	pm.OnDisconnect(func() {
		fmt.Fprintln(os.Stderr, "\nP2P 連線已中斷")
		cancel()
	})

	adbAddr := fmt.Sprintf("127.0.0.1:%d", *adbPort)
	handler := &bridge.ServerHandler{ADBAddr: adbAddr}

	// 設定 DataChannel 處理
	pm.OnChannel(func(label string, rwc io.ReadWriteCloser) {
		if label == "control" {
			// control channel — 啟動設備推送
			go bridge.DevicePushLoop(ctx, rwc, adbAddr, func(devices []bridge.DeviceInfo) {
				online := 0
				for _, d := range devices {
					if d.State == "device" {
						online++
					}
				}
				fmt.Fprintf(os.Stderr, "在線設備: %d 台\n", online)
			})
			return
		}
		// 其他 DataChannel 由 ServerHandler 分派
		if !handler.HandleChannel(ctx, label, rwc) {
			slog.Warn("unknown DataChannel label", "label", label)
			rwc.Close()
		}
	})

	// 處理 Offer
	fmt.Fprintln(os.Stderr, "正在處理邀請碼並收集 ICE 候選...")
	answerSDP, err := pm.HandleOffer(offerSDP)
	if err != nil {
		fmt.Fprintf(os.Stderr, "處理 Offer 失敗: %v\n", err)
		os.Exit(1)
	}

	// 產生 compact SDP answer
	answerCompact := bridge.SDPToCompact(answerSDP)
	answerTokenStr, err := bridge.EncodeToken(answerCompact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "編碼回應碼失敗: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "\n回應碼（複製回主控端）:")
	fmt.Println(answerTokenStr)
	fmt.Fprintln(os.Stderr, "\n等待連線...")

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\n已停止")
}
