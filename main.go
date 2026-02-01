package main

import (
	"html/template"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

type Client struct {
	ws          *websocket.Conn
	pc          *webrtc.PeerConnection
	isInitiator bool
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	mu      sync.Mutex
	clients []*Client
)

func indexHandler(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl.Execute(w, nil)
}

func main() {
	http.HandleFunc("/call", indexHandler)
	http.HandleFunc("/ws", wsHandler)

	log.Println("SFU running on https://0.0.0.0:8080")
	log.Fatal(http.ListenAndServeTLS(":8080", "cert.pem", "key.pem", nil))
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WS upgrade error:", err)
		return
	}
	defer ws.Close()
	log.Println("New client connected")

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		log.Println("Failed to create PeerConnection:", err)
		return
	}

	client := &Client{ws: ws, pc: pc}

	mu.Lock()
	if len(clients) >= 2 {
		mu.Unlock()
		ws.WriteJSON(map[string]string{"error": "room is full"})
		return
	}
	client.isInitiator = len(clients) == 0
	clients = append(clients, client)
	mu.Unlock()

	ws.WriteJSON(map[string]any{
		"type":         "role",
		"initiator":    client.isInitiator,
		"participants": len(clients),
	})

	defer cleanup(client)

	// После создания pc
	pc.OnNegotiationNeeded(func() {
		if pc.SignalingState() != webrtc.SignalingStateStable {
			log.Println("Skipping renegotiation, signaling state:", pc.SignalingState())
			return
		}

		offer, err := pc.CreateOffer(nil)
		if err != nil {
			log.Println("CreateOffer failed:", err)
			return
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			log.Println("SetLocalDescription failed:", err)
			return
		}
		mu.Lock()
		ws.WriteJSON(offer)
		mu.Unlock()
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			mu.Lock()
			ws.WriteJSON(c.ToJSON())
			mu.Unlock()
		}
	})

	pc.OnConnectionStateChange(func(status webrtc.PeerConnectionState) {
		log.Println("Connection status:", status.String())
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		log.Println("Track received:", track.Kind())

		mu.Lock()
		defer mu.Unlock()

		for _, other := range clients {
			if other == client {
				continue // не отправляем себе
			}

			// Проверяем, что трек ещё не добавлен
			alreadyAdded := false
			for _, sender := range other.pc.GetSenders() {
				if sender.Track() != nil && sender.Track().ID() == track.ID()+"-"+other.ws.RemoteAddr().String() {
					alreadyAdded = true
					break
				}
			}
			if alreadyAdded {
				continue
			}

			// Создаём уникальные TrackID и StreamID для каждого клиента
			trackID := track.ID() + "-" + other.ws.RemoteAddr().String()
			streamID := track.StreamID() + "-" + other.ws.RemoteAddr().String()

			localTrack, err := webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, trackID, streamID)
			if err != nil {
				log.Println("Error creating local track:", err)
				continue
			}

			_, err = other.pc.AddTrack(localTrack)
			if err != nil {
				log.Println("Error adding track:", err)
				continue
			}

			go func(tRemote *webrtc.TrackRemote, tLocal *webrtc.TrackLocalStaticRTP) {
				buf := make([]byte, 1500)
				for {
					n, _, err := tRemote.Read(buf)
					if err != nil {
						log.Println("Read:", err)
						return
					}
					if _, err := tLocal.Write(buf[:n]); err != nil {
						log.Println("Write:", err)
						return
					}
				}
			}(track, localTrack)
		}
	})

	for {
		var msg map[string]any
		if err := ws.ReadJSON(&msg); err != nil {
			log.Println("ReadJSON:", err)
			return
		}

		if msg["type"] != nil {
			sdp := webrtc.SessionDescription{
				Type: webrtc.NewSDPType(msg["type"].(string)),
				SDP:  msg["sdp"].(string),
			}

			if err = pc.SetRemoteDescription(sdp); err != nil {
				log.Println("SetRemoteDescription:", err)
			}

			if sdp.Type == webrtc.SDPTypeOffer {
				answer, err := pc.CreateAnswer(nil)
				if err != nil {
					log.Println("CreateAnswer:", err)
				}

				if err = pc.SetLocalDescription(answer); err != nil {
					log.Println("SetLocalDescription:", err)
				}

				mu.Lock()
				if err = ws.WriteJSON(answer); err != nil {
					log.Println("WriteJSON:", err)
				}
				mu.Unlock()
			}
		} else if msg["candidate"] != nil {
			err = pc.AddICECandidate(webrtc.ICECandidateInit{
				Candidate: msg["candidate"].(string),
			})
			if err != nil {
				log.Println("AddICECandidate:", err)
			}
		}
	}
}

func renegotiate(c *Client) {
	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		log.Println("Failed to create offer:", err)
		return
	}

	if err = c.pc.SetLocalDescription(offer); err != nil {
		log.Println("SetLocalDescription:", err)
	}

	if err = c.ws.WriteJSON(offer); err != nil {
		log.Println("WriteJSON:", err)
	}
}

func cleanup(c *Client) {
	mu.Lock()
	defer mu.Unlock()
	log.Println("Client disconnected")

	c.pc.Close()
	c.ws.Close()

	for i, cl := range clients {
		if cl == c {
			clients = append(clients[:i], clients[i+1:]...)
			break
		}
	}
}
