// SPDX-FileCopyrightText: 2025 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

// ice-proxy demonstrates Pion WebRTC's proxy abilities.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
	"golang.org/x/net/websocket"
)

var api *webrtc.API //nolint

// nolint: gocognit, cyclop
func websocketServer(wsConn *websocket.Conn) {
	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs:       []string{"turn:127.0.0.1:17342?transport=tcp"},
				Username:   "turn_username",
				Credential: "turn_password",
			},
		},
		// This forces to use TURN instead of direct connection.
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	})
	if err != nil {
		panic(err)
	}

	// When Pion gathers a new ICE Candidate send it to the client. This is how
	// ice trickle is implemented. Everytime we have a new candidate available we send
	// it as soon as it is ready. We don't wait to emit a Offer/Answer until they are
	// all available
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		outbound, marshalErr := json.Marshal(candidate.ToJSON())
		if marshalErr != nil {
			panic(marshalErr)
		}

		if _, err = wsConn.Write(outbound); err != nil {
			panic(err)
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())
	})

	// Send the current time via a DataChannel to the remote peer every 3 seconds
	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnOpen(func() {
			for range time.Tick(time.Second * 3) {
				if err = d.SendText(time.Now().String()); err != nil {
					panic(err)
				}
			}
		})
	})

	buf := make([]byte, 1500)
	for {
		// Read each inbound WebSocket Message
		n, err := wsConn.Read(buf)
		if err != nil {
			panic(err)
		}

		// Unmarshal each inbound WebSocket message
		var (
			candidate webrtc.ICECandidateInit
			offer     webrtc.SessionDescription
		)

		switch {
		// Attempt to unmarshal as a SessionDescription. If the SDP field is empty
		// assume it is not one.
		case json.Unmarshal(buf[:n], &offer) == nil && offer.SDP != "":
			if err = peerConnection.SetRemoteDescription(offer); err != nil {
				panic(err)
			}

			answer, answerErr := peerConnection.CreateAnswer(nil)
			if answerErr != nil {
				panic(answerErr)
			}

			if err = peerConnection.SetLocalDescription(answer); err != nil {
				panic(err)
			}

			outbound, marshalErr := json.Marshal(answer)
			if marshalErr != nil {
				panic(marshalErr)
			}

			if _, err = wsConn.Write(outbound); err != nil {
				panic(err)
			}
		// Attempt to unmarshal as a ICECandidateInit. If the candidate field is empty
		// assume it is not one.
		case json.Unmarshal(buf[:n], &candidate) == nil && candidate.Candidate != "":
			log.Printf("Receive new candidate: %s", candidate.Candidate)
			if err = peerConnection.AddICECandidate(candidate); err != nil {
				panic(err)
			}
		default:
			panic("Unknown message")
		}
	}
}

func main() {
	// Setup TURN
	turnServer := newTURNServer()
	defer func() {
		if err := turnServer.Close(); err != nil {
			log.Printf("close turn server: %v", err)
		}
	}()

	// Setup proxy
	proxyURL, proxyListener := newHTTPProxy()
	defer func() {
		if err := proxyListener.Close(); err != nil {
			log.Printf("close proxy listener: %v", err)
		}
	}()

	// Setup proxy dialer
	proxyDialer := newProxyDialer(proxyURL)

	// Set proxy dialer, works only for TURN + TCP
	var settingEngine webrtc.SettingEngine
	settingEngine.SetICEProxyDialer(proxyDialer)
	lf := logging.NewDefaultLoggerFactory()
	lf.DefaultLogLevel = logging.LogLevelWarn
	settingEngine.LoggerFactory = lf
	settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	api = webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))
	http.Handle("/", http.FileServer(http.Dir(".")))
	http.Handle("/websocket", websocket.Handler(websocketServer))

	fmt.Println("Open http://172.16.242.243:8080 to access this demo")
	// nolint: gosec
	panic(http.ListenAndServe(":8080", nil))
}
