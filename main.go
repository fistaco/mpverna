package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/gorilla/websocket"
)

const pipeName = `\\.\pipe\mpv-subs`

var targetLanguage = "Chinese"

// maxint
const generatedSubtitleTrackIndex = -1
const convertedSubtitleTrackIndex = -2

// ======================================================
// Subtitle structures
// ======================================================

type Line struct {
	Start float64
	End   float64
	Text  string
}

type Track struct {
	Name          string
	Lang          string
	File          string
	Subs          []Line
	IsExternal    bool
	SubTrackIndex int
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
					setActiveTrack(id, tracks[id])
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
	// Log initially found tracks
	log.Printf("Found %d tracks in MPV:\n", len(trackList))
	for i, t := range trackList {
		m := t.(map[string]interface{})
		log.Printf("Track %d: type=%s, title=%s, lang=%s\n", i, m["type"], m["title"], m["lang"])
	}

	for _, t := range trackList {
		m := t.(map[string]interface{})

		if m["type"] != "sub" {
			continue
		}

		// Currently only supports external subtitle files, embedded subs would require diferent logic
		external_filename, isExternal := m["external-filename"].(string)
		// if !ok {
		// 	continue
		// }

		if !filepath.IsAbs(external_filename) {
			external_filename = filepath.Join(videoDir, external_filename)
		}

		tracks = append(tracks, Track{
			Name:          fmt.Sprint(m["title"]),
			Lang:          fmt.Sprint(m["lang"]),
			File:          external_filename,
			Subs:          loadSRT(external_filename),
			IsExternal:    isExternal,
			SubTrackIndex: int(m["ff-index"].(float64)),
		})
	}
}

func setActiveTrack(trackIndex int, track Track) {
	log.Printf("Setting active track to %d (name: %s)", trackIndex, track.Name)
	activeTrack = trackIndex
}

func selectActiveTrack(language string) {
	log.Println("Selecting active subtitle track to display on the web page...")
	if len(tracks) == 0 {
		log.Println("No subtitle tracks available to select.")
		return
	}

	videoPath := getFullMpvVideoPath()
	// Select a track if it was previously generated or converted by this server for the target language
	generatedSubtitleFilename := constructGeneratedSrtFilename(videoPath, language)
	convertedSubtitleFilename := constructConvertedSrtFilename(videoPath, language)
	for i, t := range tracks {
		if t.Name == generatedSubtitleFilename || t.Name == convertedSubtitleFilename {
			setActiveTrack(i, t)
			return
		}
	}

	// If an embedded track matches the target language, convert it to SRT and load that SRT file as a new track
	for _, t := range tracks {
		l := strings.ToLower(t.Lang)
		// TODO: Support other languages and language code formats
		if strings.HasPrefix(l, "zh") || strings.HasPrefix(l, "chi") || strings.HasPrefix(l, "zho") {
			convertEmbeddedSubToSrt(videoPath, language, t.SubTrackIndex)
			convertedSrtPath := filepath.Join(filepath.Dir(videoPath), convertedSubtitleFilename)
			tracks = append(tracks, Track{
				Name:          convertedSubtitleFilename,
				Lang:          language,
				File:          convertedSrtPath,
				Subs:          loadSRT(convertedSrtPath),
				IsExternal:    true,
				SubTrackIndex: convertedSubtitleTrackIndex,
			})
			selectActiveTrack(language)
		}
	}

	// No embedded subs or external SRTs matching the target language were found, so generate new subs and select those
	err := generateTtsSrt(videoPath, language)
	generatedSubtitleFilepath := filepath.Join(filepath.Dir(videoPath), generatedSubtitleFilename)
	tracks = append(tracks, Track{
		Name:          generatedSubtitleFilename,
		Lang:          language,
		File:          generatedSubtitleFilepath,
		Subs:          loadSRT(generatedSubtitleFilepath),
		IsExternal:    true,
		SubTrackIndex: generatedSubtitleTrackIndex,
	})
	selectActiveTrack(language)
	if err == nil {
		return
	}

	// Use the first external track by default if no better options are found
	for i, t := range tracks {
		if t.IsExternal {
			setActiveTrack(i, t)
			return
		}
	}

	// Fall back to 0 if all else fails
	setActiveTrack(0, tracks[0])
}

func runCmdWithOutputStreams(cmd *exec.Cmd) error {
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1")
	stdout, err := cmd.StdoutPipe()
	stderr, err := cmd.StderrPipe()
	cmd.Start()
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			fmt.Println("STDOUT:", scanner.Text())
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			fmt.Println("STDERR:", scanner.Text())
		}
	}()
	cmd.Wait()

	return err
}

// Generates an SRT file using Whisper text-to-speech for the given language and video file
func generateTtsSrt(videoPath string, language string) error {
	log.Println("Generating SRT file using Whisper text-to-speech...")

	cmd := exec.Command("whisper", videoPath, "--model", "turbo", "--language", language, "--output_format", "srt", "--device", "cuda", "--fp16", "True", "--output_dir", filepath.Dir(videoPath))
	log.Printf("Running command %s", strings.Join(cmd.Args, " "))
	// err := runCmdWithOutputStreams(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1")
	stdout, err := cmd.StdoutPipe()
	stderr, err := cmd.StderrPipe()
	cmd.Start()
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			fmt.Println("STDOUT:", scanner.Text())
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			fmt.Println("STDERR:", scanner.Text())
		}
	}()
	cmd.Wait()

	if err != nil {
		log.Printf("Failed to generate SRT file using Whisper: %s", err)
		return err
	}

	subsFilepath := strings.Replace(videoPath, ".mkv", ".srt", 1)
	if _, err := os.Stat(subsFilepath); os.IsNotExist(err) {
		log.Printf("Whisper did not generate the expected SRT file: %s", subsFilepath)
		return err
	}

	// Rename the generated file so we can find it later
	generatedSubtitleFilename := constructGeneratedSrtFilename(videoPath, language)
	generatedSubtitleFilepath := filepath.Join(filepath.Dir(videoPath), generatedSubtitleFilename)
	err = os.Rename(subsFilepath, generatedSubtitleFilepath)

	// send("sub-add", subsFilepath)
	return err
}

func convertEmbeddedSubToSrt(videoPath string, language string, subTrackIndex int) {
	log.Println("Converting embedded subtitle track to SRT using ffmpeg...")

	dir := filepath.Dir(videoPath)
	convertedSrtFilepath := filepath.Join(dir, constructConvertedSrtFilename(videoPath, language))

	cmd := exec.Command("ffmpeg", "-i", videoPath, "-map", "0:s:"+fmt.Sprint(subTrackIndex), convertedSrtFilepath)
	err := runCmdWithOutputStreams(cmd)
	if err != nil {
		log.Printf("Failed to convert embedded subtitle to SRT: %v", err)
	}
}

// Constructs a filename for an SRT file generated by this project based on the video filename and
// language. The format is "VIDEONAME-LANGUAGE-GENERATED.srt" and does not contain the full filepath.
func constructGeneratedSrtFilename(videoPath string, language string) string {
	base := filepath.Base(videoPath)
	base_without_ext := strings.TrimSuffix(base, filepath.Ext(base))
	language_normalized := strings.ToUpper(language)

	return base_without_ext + "-" + language_normalized + "-GENERATED.srt"
}

// Constructs a filename for an SRT file converted from an embedded subtitle track using ffmpeg
func constructConvertedSrtFilename(videoPath string, language string) string {
	base := filepath.Base(videoPath)
	base_without_ext := strings.TrimSuffix(base, filepath.Ext(base))
	language_normalized := strings.ToUpper(language)

	return base_without_ext + "-" + language_normalized + "-CONVERTED.srt"
}

// Determines whether a corresponding .srt subtitle file exists for the given video path and language
func subFileExists(videoPath string, language string) bool {
	srtPath := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + ".srt"
	if _, err := os.Stat(srtPath); err == nil {
		return true
	}

	return false
}

func waitForMPV() {
	// TODO: Implement
}

func getFullMpvVideoPath() string {
	videoPath := getProperty("path").(string)
	mpvWD := getProperty("working-directory").(string)

	var absVideoPath string

	if filepath.IsAbs(videoPath) {
		absVideoPath = videoPath
	} else {
		absVideoPath = filepath.Join(mpvWD, videoPath)
	}

	return absVideoPath
}

func listenMPV() {
	var err error
	conn, err = winio.DialPipe(pipeName, nil)
	if err != nil {
		// Wait for the pipe to be available if mpv hasn't been launched yet
		log.Println("Waiting for mpv to launch...")
		for {
			conn, err = winio.DialPipe(pipeName, nil)
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	log.Println("Found MPV IPC pipe")
	reader = bufio.NewReader(conn)

	absVideoPath := getFullMpvVideoPath()
	videoDir := filepath.Dir(absVideoPath)

	raw := getProperty("track-list").([]interface{})
	loadTracks(raw, videoDir)
	selectActiveTrack(targetLanguage)

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
			if (t.IsExternal) {
				const o=document.createElement("option");
				o.value=i;
				o.textContent=(t.Lang||"")+" "+(t.Name||"");
				sel.appendChild(o);
			}
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

func pargseArgs() {
	if len(os.Args) > 1 {
		targetLanguage = os.Args[1]
	}
}

func main() {
	pargseArgs()

	// The command `mpv.exe --input-ipc-server=\\.\pipe\mpv-subs video.mkv` should already be running
	go listenMPV()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page)
	})
	http.HandleFunc("/ws", wsHandler)

	log.Println("http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}
