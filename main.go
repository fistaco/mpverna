package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Microsoft/go-winio"
	"github.com/gorilla/websocket"
)

const pipeName = `\\.\pipe\mpv-subs`

// ======================================================
// Subtitle structures
// ======================================================

type Line struct {
	Start float64
	End   float64
	Text  string
}

type Track struct {
	Name string
	Lang string
	File string
	Subs []Line
}

var tracks []Track
var activeTrack = 0

// ======================================================
// SRT parser
// ======================================================

func parseTime(t string) float64 {
	t = strings.ReplaceAll(t, ",", ".")
	p := strings.Split(t, ":")
	h, _ := strconv.ParseFloat(p[0], 64)
	m, _ := strconv.ParseFloat(p[1], 64)
	s, _ := strconv.ParseFloat(p[2], 64)
	return h*3600 + m*60 + s
}

func loadSRT(path string) []Line {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	var out []Line
	sc := bufio.NewScanner(f)

	for {
		if !sc.Scan() {
			break
		}

		if !sc.Scan() {
			break
		}
		times := sc.Text()

		parts := strings.Split(times, " --> ")
		start := parseTime(parts[0])
		end := parseTime(parts[1])

		var text []string
		for sc.Scan() {
			l := sc.Text()
			if l == "" {
				break
			}
			text = append(text, l)
		}

		out = append(out, Line{start, end, strings.Join(text, "\n")})
	}

	return out
}

func findLine(subs []Line, t float64) string {
	lo, hi := 0, len(subs)-1
	for lo <= hi {
		m := (lo + hi) / 2
		l := subs[m]
		if t < l.Start {
			hi = m - 1
		} else if t > l.End {
			lo = m + 1
		} else {
			return l.Text
		}
	}
	return ""
}

// ======================================================
// Websocket
// ======================================================

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
var clients = map[*websocket.Conn]bool{}
var mu sync.Mutex

func broadcast(text string) {
	mu.Lock()
	defer mu.Unlock()
	for c := range clients {
		c.WriteMessage(websocket.TextMessage, []byte(text))
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	c, _ := upgrader.Upgrade(w, r, nil)

	mu.Lock()
	clients[c] = true

	// send track list immediately
	j, _ := json.Marshal(tracks)
	c.WriteMessage(websocket.TextMessage, []byte("TRACKS:"+string(j)))
	mu.Unlock()

	go func() {
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if strings.HasPrefix(string(msg), "SET:") {
				id, _ := strconv.Atoi(strings.TrimPrefix(string(msg), "SET:"))
				if id >= 0 && id < len(tracks) {
					activeTrack = id
				}
			}
		}
	}()
}

// ======================================================
// MPV IPC
// ======================================================

var conn net.Conn
var reader *bufio.Reader

func send(cmd ...interface{}) {
	j, _ := json.Marshal(map[string]interface{}{"command": cmd})
	conn.Write(append(j, '\n'))
}

func getProperty(name string) interface{} {
	send("get_property", name)
	for {
		line, _ := reader.ReadBytes('\n')
		var resp map[string]interface{}
		json.Unmarshal(line, &resp)
		if d, ok := resp["data"]; ok {
			return d
		}
	}
}

func loadTracks(trackList []interface{}, videoDir string) {
	for _, t := range trackList {
		m := t.(map[string]interface{})

		if m["type"] != "sub" {
			continue
		}

		file, ok := m["external-filename"].(string)
		if !ok {
			continue
		}

		if !filepath.IsAbs(file) {
			file = filepath.Join(videoDir, file)
		}

		tracks = append(tracks, Track{
			Name: fmt.Sprint(m["title"]),
			Lang: fmt.Sprint(m["lang"]),
			File: file,
			Subs: loadSRT(file),
		})
	}

	if len(tracks) == 0 {
		log.Fatal("No external SRT subtitles found")
	}

	// Chinese default else first
	for i, t := range tracks {
		l := strings.ToLower(t.Lang)
		if strings.HasPrefix(l, "zh") ||
			strings.HasPrefix(l, "chi") ||
			strings.HasPrefix(l, "zho") {
			activeTrack = i
			return
		}
	}
	activeTrack = 0
}

func listenMPV() {
	var err error
	conn, err = winio.DialPipe(pipeName, nil)
	if err != nil {
		log.Fatal(err)
	}
	reader = bufio.NewReader(conn)

	videoPath := getProperty("path").(string)
	mpvWD := getProperty("working-directory").(string)

	var absVideoPath string

	if filepath.IsAbs(videoPath) {
		absVideoPath = videoPath
	} else {
		absVideoPath = filepath.Join(mpvWD, videoPath)
	}

	videoDir := filepath.Dir(absVideoPath)
	raw := getProperty("track-list").([]interface{})
	loadTracks(raw, videoDir)

	send("observe_property", 1, "time-pos")

	for {
		line, _ := reader.ReadBytes('\n')

		var msg map[string]interface{}
		json.Unmarshal(line, &msg)

		if msg["event"] == "property-change" &&
			msg["name"] == "time-pos" {

			if t, ok := msg["data"].(float64); ok {
				txt := findLine(tracks[activeTrack].Subs, t)
				broadcast(txt)
			}
		}
	}
}

// ======================================================
// Web UI
// ======================================================

const page = `
<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<style>
html,body{height:100%;margin:0}
body{
	background:#000;color:#fff;
	display:flex;flex-direction:column;
	justify-content:center;align-items:center;
	font-family:sans-serif;text-align:center;
}
#subs{font-size:42px;margin-top:20px;user-select:text}
select{background:#111;color:#fff;border:1px solid #555;padding:6px}
</style>
</head>
<body>
<select id="sel"></select>
<div id="subs"></div>

<script>
const ws=new WebSocket("ws://localhost:8080/ws");
const sel=document.getElementById("sel");
const subs=document.getElementById("subs");

ws.onmessage=e=>{
	if(e.data.startsWith("TRACKS:")){
		const tracks=JSON.parse(e.data.slice(7));
		tracks.forEach((t,i)=>{
			const o=document.createElement("option");
			o.value=i;
			o.textContent=(t.lang||"")+" "+(t.name||"");
			sel.appendChild(o);
		});
		return;
	}
	subs.textContent=e.data;
};

sel.onchange=()=>ws.send("SET:"+sel.value);
</script>
</body>
</html>
`

// ======================================================

func main() {
	go listenMPV()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page)
	})
	http.HandleFunc("/ws", wsHandler)

	log.Println("http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}
