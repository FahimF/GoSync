package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	Port         = ":8080"
	DataDir      = "./data"
	MetadataFile = "./metadata.json"
	MaxLogs      = 100
)

// --- Data Structures ---

type FileMetadata struct {
	Path      string `json:"path"`
	Hash      string `json:"hash"`
	UpdatedAt int64  `json:"updatedAt"`
}

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	Level     string    `json:"level"` // INFO, ERROR, CONNECT
}

type ClientInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	IP         string    `json:"ip"`
	Pn         string    `json:"pn"` // Plugin Name/Version if available
	ConnectedAt time.Time `json:"connectedAt"`
}

type ServerState struct {
	mu      sync.RWMutex
	Files   map[string]FileMetadata
	Clients map[*websocket.Conn]*ClientInfo
	Logs    []LogEntry
}

// --- Globals ---

var state = ServerState{
	Files:   make(map[string]FileMetadata),
	Clients: make(map[*websocket.Conn]*ClientInfo),
	Logs:    make([]LogEntry, 0),
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true 
	},
}

var broadcast = make(chan interface{})

// --- Main ---

func main() {
	// Ensure data directory exists
	if err := os.MkdirAll(DataDir, 0755); err != nil {
		log.Fatal("Failed to create data directory:", err)
	}

	loadMetadata()
	addLog("INFO", "Server started. Loading metadata...")

	go handleMessages()

	// API
	http.HandleFunc("/api/files", handleListFiles)
	http.HandleFunc("/api/file", handleFileOperations)
	http.HandleFunc("/api/status", handleServerStatus)
	
	// WebSocket
	http.HandleFunc("/ws", handleConnections)

	// Web Interface
	http.HandleFunc("/", handleDashboard)

	addLog("INFO", fmt.Sprintf("GoSync Server listening on %s", Port))
	fmt.Printf("GoSync Server started at http://localhost%s\n", Port)
	
	log.Fatal(http.ListenAndServe(Port, nil))
}

// --- Logging & State Helpers ---

func addLog(level, message string) {
	state.mu.Lock()
	defer state.mu.Unlock()

	entry := LogEntry{
		Timestamp: time.Now(),
		Message:   message,
		Level:     level,
	}
	
	// Prepend or Append? Append is standard, UI can reverse.
	state.Logs = append(state.Logs, entry)
	if len(state.Logs) > MaxLogs {
		state.Logs = state.Logs[1:]
	}
	
	// Also print to stdout
	fmt.Printf("[%s] %s: %s\n", entry.Timestamp.Format("15:04:05"), level, message)
}

// --- HTTP Handlers ---

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, dashboardHTML)
}

func handleServerStatus(w http.ResponseWriter, r *http.Request) {
	state.mu.RLock()
	defer state.mu.RUnlock()

	// Convert Clients map to list for JSON
	clientList := make([]*ClientInfo, 0, len(state.Clients))
	for _, c := range state.Clients {
		clientList = append(clientList, c)
	}

	// Files list summary
	fileCount := len(state.Files)

	response := map[string]interface{}{
		"clients":   clientList,
		"logs":      state.Logs,
		"fileCount": fileCount,
		"uptime":    "Not tracked", // Could add
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		addLog("ERROR", fmt.Sprintf("WebSocket Upgrade error: %v", err))
		return
	}
	defer ws.Close()

	clientIP := r.RemoteAddr
	
	// Wait for identification
	var identityMsg struct {
		Type       string `json:"type"`
		DeviceName string `json:"deviceName"`
	}
	
	// Set read deadline for identification to avoid hanging connections
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	err = ws.ReadJSON(&identityMsg)
	ws.SetReadDeadline(time.Time{}) // Reset deadline

	deviceName := "Unknown"
	if err == nil && identityMsg.Type == "identify" {
		deviceName = identityMsg.DeviceName
	} else {
		// If no ID sent immediately, we might just log it and proceed (backward compatibility?)
		// For now, let's assume valid clients update.
		if err != nil {
			addLog("WARN", fmt.Sprintf("Client at %s failed to identify: %v", clientIP, err))
		}
	}

	info := &ClientInfo{
		ID:          fmt.Sprintf("%d", time.Now().UnixNano()),
		Name:        deviceName,
		IP:          clientIP,
		ConnectedAt: time.Now(),
	}

	state.mu.Lock()
	state.Clients[ws] = info
	state.mu.Unlock()

	addLog("CONNECT", fmt.Sprintf("Client connected: %s (%s)", deviceName, clientIP))

	// Standard loop
	for {
		var msg map[string]interface{}
		err := ws.ReadJSON(&msg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnrormalClosure) {
				addLog("ERROR", fmt.Sprintf("Client %s error: %v", deviceName, err))
			} else {
				addLog("CONNECT", fmt.Sprintf("Client disconnected: %s", deviceName))
			}
			break
		}
		
		// Handle other messages if needed (e.g. explicit sync request)
		if msgType, ok := msg["type"].(string); ok {
			addLog("INFO", fmt.Sprintf("Received message from %s: %s", deviceName, msgType))
		}
	}

	state.mu.Lock()
	delete(state.Clients, ws)
	state.mu.Unlock()
}

func handleMessages() {
	for {
		msg := <-broadcast
		state.mu.RLock()
		for client := range state.Clients {
			err := client.WriteJSON(msg)
			if err != nil {
				log.Printf("WebSocket write error: %v", err)
				client.Close()
				// Deletion happens in read loop
			}
		}
		state.mu.RUnlock()
	}
}

func handleListFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	list := make([]FileMetadata, 0, len(state.Files))
	for _, meta := range state.Files {
		list = append(list, meta)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func handleFileOperations(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "Missing 'path' query parameter", http.StatusBadRequest)
		return
	}

	// Prevent directory traversal
	cleanPath := filepath.Join("/", filePath) 
	localPath := filepath.Join(DataDir, cleanPath)

	if r.Method == http.MethodGet {
		// Download
		addLog("INFO", fmt.Sprintf("Serving file: %s", filePath))
		http.ServeFile(w, r, localPath)
		return
	}

	if r.Method == http.MethodPut {
		// Upload
		addLog("INFO", fmt.Sprintf("Receiving file: %s", filePath))
		
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			addLog("ERROR", fmt.Sprintf("Failed to create dir for %s: %v", filePath, err))
			http.Error(w, "Failed to create directory", http.StatusInternalServerError)
			return
		}

		outFile, err := os.Create(localPath)
		if err != nil {
			addLog("ERROR", fmt.Sprintf("Failed to create file %s: %v", filePath, err))
			http.Error(w, "Failed to create file", http.StatusInternalServerError)
			return
		}
		defer outFile.Close()

		hasher := sha256.New()
		reader := io.TeeReader(r.Body, hasher)

		if _, err := io.Copy(outFile, reader); err != nil {
			addLog("ERROR", fmt.Sprintf("Failed to write file %s: %v", filePath, err))
			http.Error(w, "Failed to write file", http.StatusInternalServerError)
			return
		}

		hash := hex.EncodeToString(hasher.Sum(nil))

		meta := FileMetadata{
			Path:      filePath,
			Hash:      hash,
			UpdatedAt: time.Now().UnixMilli(),
		}

		state.mu.Lock()
		state.Files[filePath] = meta
		saveMetadata()
		state.mu.Unlock()

		addLog("INFO", fmt.Sprintf("File updated: %s", filePath))

		broadcast <- map[string]interface{}{
			"type": "file_updated",
			"file": meta,
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(meta)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func loadMetadata() {
	state.mu.Lock()
	defer state.mu.Unlock()

	data, err := os.ReadFile(MetadataFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Println("Error reading metadata:", err)
		}
		return
	}

	var fileList []FileMetadata
	if err := json.Unmarshal(data, &fileList); err != nil {
		log.Println("Error parsing metadata:", err)
		return
	}

	for _, f := range fileList {
		state.Files[f.Path] = f
	}
}

func saveMetadata() {
	list := make([]FileMetadata, 0, len(state.Files))
	for _, meta := range state.Files {
		list = append(list, meta)
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		log.Println("Error marshalling metadata:", err)
		return
	}

	if err := os.WriteFile(MetadataFile, data, 0644); err != nil {
		log.Println("Error writing metadata:", err)
	}
}

const dashboardHTML = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>GoSync Server Dashboard</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; margin: 0; padding: 20px; background: #f4f4f9; color: #333; }
        .container { max-width: 1000px; margin: 0 auto; }
        .card { background: white; padding: 20px; border-radius: 8px; box-shadow: 0 2px 5px rgba(0,0,0,0.1); margin-bottom: 20px; }
        h2 { margin-top: 0; }
        table { width: 100%; border-collapse: collapse; }
        th, td { padding: 10px; text-align: left; border-bottom: 1px solid #eee; }
        th { background: #fafafa; }
        .log-entry { font-family: monospace; padding: 5px 0; border-bottom: 1px solid #f0f0f0; }
        .log-INFO { color: #0052cc; }
        .log-ERROR { color: #cc0000; }
        .log-CONNECT { color: #008000; }
        .status-badge { display: inline-block; padding: 4px 8px; border-radius: 4px; font-size: 12px; font-weight: bold; }
        .status-online { background: #e3fcef; color: #006644; }
    </style>
</head>
<body>
    <div class="container">
        <h1>GoSync Server</h1>
        
        <div class="card">
            <h2>Connected Devices</h2>
            <table id="clientsTable">
                <thead><tr><th>Name</th><th>IP</th><th>Connected At</th></tr></thead>
                <tbody><!-- JS will populate --></tbody>
            </table>
        </div>

        <div class="card">
            <h2>Stats</h2>
            <p>Total Files Tracked: <span id="fileCount">0</span></p>
        </div>

        <div class="card">
            <h2>Server Logs</h2>
            <div id="logs" style="max-height: 400px; overflow-y: auto;"></div>
        </div>
    </div>

    <script>
        function fetchStatus() {
            fetch('/api/status')
                .then(r => r.json())
                .then(data => {
                    // Clients
                    const tbody = document.querySelector('#clientsTable tbody');
                    tbody.innerHTML = data.clients.map(c => 
                        '<tr><td>' + c.name + '</td><td>' + c.ip + '</td><td>' + new Date(c.connectedAt).toLocaleTimeString() + '</td></tr>'
                    ).join('');

                    // Stats
                    document.getElementById('fileCount').textContent = data.fileCount;

                    // Logs
                    const logsDiv = document.getElementById('logs');
                    logsDiv.innerHTML = data.logs.slice().reverse().map(l => 
                        '<div class="log-entry log-' + l.level + '">
' + 
                        '[' + new Date(l.timestamp).toLocaleTimeString() + '] ' + l.message + 
                        '</div>'
                    ).join('');
                });
        }
        
        setInterval(fetchStatus, 2000);
        fetchStatus();
    </script>
</body>
</html>