package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/Microsoft/go-winio"
	"github.com/gorilla/websocket"
)

const pipeName = `\\.\pipe\mpv-subs`

// ======================================================
// Types
// ======================================================

type Track struct {
	ID    int    `json:"id"`
	Type  string `json:"type"`
	Lang  string `json:"lang"`
	Title string `json:"title"`
}

type rpc struct {
	Command []interface{} `json:"command"`
}

// ======================================================
// Websocket hub
// ======================================================

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

var clients = make(map[*websocket.Conn]bool)
var mu sync.Mutex

func broadcast(msg string) {
	mu.Lock()
	defer mu.Unlock()
	for c := range clients {
		c.WriteMessage(websocket.TextMessage, []byte(msg))
	}
}

// ======================================================
// MPV IPC helpers
// ======================================================

var mpvConn net.Conn
var reader *bufio.Reader

func send(cmd ...interface{}) {
	j, _ := json.Marshal(rpc{Command: cmd})
	mpvConn.Write(append(j, '\n'))
}

func getProperty(name string) interface{} {
	req := rpc{Command: []interface{}{"get_property", name}}
	j, _ := json.Marshal(req)
	mpvConn.Write(append(j, '\n'))

	for {
		line, _ := reader.ReadBytes('\n')
		var resp map[string]interface{}
		json.Unmarshal(line, &resp)

		if data, ok := resp["data"]; ok {
			return data
		}
	}
}

var cachedTracks string

// ======================================================
// Track logic
// ======================================================

func chooseDefaultTrack(tracks []Track) int {
	first := -1

	for _, t := range tracks {
		if t.Type != "sub" {
			continue
		}

		if first == -1 {
			first = t.ID
		}

		lang := strings.ToLower(t.Lang)

		// Chinese preference
		if strings.HasPrefix(lang, "zh") ||
			strings.HasPrefix(lang, "chi") ||
			strings.HasPrefix(lang, "zho") {
			return t.ID
		}
	}

	return first
}

// ======================================================
// MPV listener
// ======================================================

func listenMPV() {
	var err error
	mpvConn, err = winio.DialPipe(pipeName, nil)
	if err != nil {
		log.Fatal(err)
	}

	reader = bufio.NewReader(mpvConn)

	// ---- get tracks ----
	raw := getProperty("track-list")
	bytes, _ := json.Marshal(raw)

	var tracks []Track
	json.Unmarshal(bytes, &tracks)

	def := chooseDefaultTrack(tracks)
	if def >= 0 {
		send("set_property", "sub", def)
	}

	// send track list to browser
	js, _ := json.Marshal(tracks)
	cachedTracks = "TRACKS:" + string(js)
	broadcast(cachedTracks)

	// observe subtitles
	send("observe_property", 1, "sub-text")

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}

		var msg map[string]interface{}
		json.Unmarshal(line, &msg)

		if msg["event"] == "property-change" &&
			msg["name"] == "sub-text" {

			if txt, ok := msg["data"].(string); ok {
				broadcast(txt)
			}
		}
	}
}

// ======================================================
// HTTP
// ======================================================

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, _ := upgrader.Upgrade(w, r, nil)

	mu.Lock()
	clients[conn] = true

	// immediately send cached tracks to new client
	if cachedTracks != "" {
		conn.WriteMessage(websocket.TextMessage, []byte(cachedTracks))
	}
	mu.Unlock()

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}

			if strings.HasPrefix(string(msg), "SET:") {
				id := strings.TrimPrefix(string(msg), "SET:")
				send("set_property", "sub", id)
			}
		}
	}()
}

func pageHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, htmlPage)
}

// ======================================================
// UI (black background + centered + selector)
// ======================================================

const htmlPage = `
<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>MPV Subtitles</title>

<style>
html, body {
	height:100%;
	margin:0;
}

body {
	background:#000;
	color:#fff;

	display:flex;
	flex-direction:column;
	justify-content:center;
	align-items:center;

	font-family:sans-serif;
	text-align:center;
}

#subs {
	font-size:42px;
	user-select:text;
	margin-top:20px;
	max-width:90vw;
	word-break:break-word;
}

select {
	background:#111;
	color:#fff;
	border:1px solid #555;
	padding:6px 10px;
	font-size:16px;
}
</style>
</head>

<body>

<select id="tracks"></select>
<div id="subs"></div>

<script>
const ws = new WebSocket("ws://localhost:8080/ws");
const subs = document.getElementById("subs");
const sel = document.getElementById("tracks");

ws.onmessage = (e) => {

	if (e.data.startsWith("TRACKS:")) {
		const tracks = JSON.parse(e.data.slice(7));

		tracks.filter(t=>t.type==="sub").forEach(t=>{
			const opt = document.createElement("option");
			opt.value = t.id;
			opt.textContent = (t.lang||"") + " " + (t.title||"") + " (#"+t.id+")";
			sel.appendChild(opt);
		});
		return;
	}

	subs.textContent = e.data;
};

sel.onchange = () => {
	ws.send("SET:" + sel.value);
};
</script>
</body>
</html>
`

// ======================================================

func main() {
	go listenMPV()

	http.HandleFunc("/", pageHandler)
	http.HandleFunc("/ws", wsHandler)

	fmt.Println("Open http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
