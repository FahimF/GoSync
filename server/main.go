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
	"strings"
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

type FileVersion struct {
	Hash      string `json:"hash"`
	Timestamp int64  `json:"timestamp"`
	Device    string `json:"device"`
}

type FileMetadata struct {
	Path      string        `json:"path"`
	History   []FileVersion `json:"history"` // History of changes
	Latest    FileVersion   `json:"latest"`  // Shortcut to head
}

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	Level     string    `json:"level"` // INFO, ERROR, CONNECT
}

type ClientInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	IP          string    `json:"ip"`
	Pn          string    `json:"pn"` // Plugin Name/Version if available
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
	http.HandleFunc("/api/files", enableCors(handleListFiles))
	http.HandleFunc("/api/file", enableCors(handleFileOperations))
	http.HandleFunc("/api/files/delete", enableCors(handleBulkDelete))
	http.HandleFunc("/api/search", enableCors(handleSearch))
	http.HandleFunc("/api/cleanup", enableCors(handleCleanup))
	http.HandleFunc("/api/reset", enableCors(handleReset))
	http.HandleFunc("/api/status", enableCors(handleServerStatus))
	
	// WebSocket
	http.HandleFunc("/ws", handleConnections)

	// Web Interface
	http.HandleFunc("/", handleDashboard)

	addLog("INFO", fmt.Sprintf("GoSync Server listening on %s", Port))
	fmt.Printf("GoSync Server started at http://localhost%s\n", Port)
	
	log.Fatal(http.ListenAndServe(Port, nil))
}

// --- Middleware ---

func enableCors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
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
	deviceName := "Unknown"

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
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				addLog("ERROR", fmt.Sprintf("Client %s error: %v", deviceName, err))
			} else {
				addLog("CONNECT", fmt.Sprintf("Client disconnected: %s", deviceName))
			}
			break
		}

		// Handle messages
		if msgType, ok := msg["type"].(string); ok {
			if msgType == "identify" {
				if name, ok := msg["deviceName"].(string); ok {
					deviceName = name
					
					state.mu.Lock()
					if client, exists := state.Clients[ws]; exists {
						client.Name = deviceName
					}
					state.mu.Unlock()
					
					addLog("INFO", fmt.Sprintf("Client identified as: %s", deviceName))
				}
			} else if msgType == "client_log" {
				// Handle remote logging from client
				message, _ := msg["message"].(string)
				level, _ := msg["level"].(string)
				if level == "" { level = "INFO" }
				
				// Prefix with device name for clarity
				addLog(level, fmt.Sprintf("[%s] %s", deviceName, message))
			} else {
				addLog("INFO", fmt.Sprintf("Received message from %s: %s", deviceName, msgType))
			}
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

	if r.Method == http.MethodGet {
		// Serve file
		state.mu.RLock()
		meta, exists := state.Files[filePath]
		state.mu.RUnlock()

		if !exists {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		// If specific version/hash is requested (future proofing), handle it.
		// For now, serve latest blob.
		blobPath := filepath.Join(DataDir, ".history", meta.Latest.Hash)
		
		// Fallback for migration or if direct file exists (hybrid mode)
		// Actually, let's stick to serving the blob if it exists, otherwise serve the direct file.
		if _, err := os.Stat(blobPath); err == nil {
			addLog("INFO", fmt.Sprintf("Serving blob: %s (%s)", filePath, meta.Latest.Hash))
			http.ServeFile(w, r, blobPath)
			return
		}
		
		// Fallback to direct file path (legacy support)
		cleanPath := filepath.Join("/", filePath) 
		localPath := filepath.Join(DataDir, cleanPath)
		addLog("INFO", fmt.Sprintf("Serving legacy file: %s", filePath))
		http.ServeFile(w, r, localPath)
		return
	}

	if r.Method == http.MethodPut {
		// Upload
		// 1. Save content to a temporary file to calculate hash
		tempDir := filepath.Join(DataDir, ".temp")
		if err := os.MkdirAll(tempDir, 0755); err != nil {
			http.Error(w, "Failed to create temp dir", http.StatusInternalServerError)
			return
		}
		tempFile, err := os.CreateTemp(tempDir, "upload-*")
		if err != nil {
			http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
			return
		}
		defer os.Remove(tempFile.Name()) // Cleanup temp file

		hasher := sha256.New()
		reader := io.TeeReader(r.Body, hasher)

		if _, err := io.Copy(tempFile, reader); err != nil {
			tempFile.Close()
			http.Error(w, "Failed to write temp file", http.StatusInternalServerError)
			return
		}
		tempFile.Close()

		newHash := hex.EncodeToString(hasher.Sum(nil))
		deviceName := r.Header.Get("X-Device-Name")
		if deviceName == "" {
			deviceName = "Unknown"
		}
		baseHash := r.Header.Get("X-Base-Hash")

		// 2. Check for Conflict
		state.mu.Lock()
		meta, exists := state.Files[filePath]
		isConflict := false
		
		if exists {
			// If client provides base hash, check it against latest
			if baseHash != "" && baseHash != meta.Latest.Hash {
				addLog("WARN", fmt.Sprintf("Conflict detected for %s. Client base: %s, Server head: %s", filePath, baseHash, meta.Latest.Hash))
				isConflict = true
			}
		} else {
			// New file
			meta = FileMetadata{
				Path:    filePath,
				History: make([]FileVersion, 0),
			}
		}

		// 3. Move temp file to blob storage (.history/{hash})
		blobDir := filepath.Join(DataDir, ".history")
		if err := os.MkdirAll(blobDir, 0755); err != nil {
			state.mu.Unlock()
			http.Error(w, "Failed to create history dir", http.StatusInternalServerError)
			return
		}
		blobPath := filepath.Join(blobDir, newHash)
		
		// Only move if blob doesn't exist (deduplication)
		if _, err := os.Stat(blobPath); os.IsNotExist(err) {
			if err := os.Rename(tempFile.Name(), blobPath); err != nil {
				// Fallback copy if rename fails (e.g. different devices)
				input, _ := os.ReadFile(tempFile.Name())
				os.WriteFile(blobPath, input, 0644)
			}
		}

		// 4. Update Metadata
		newVersion := FileVersion{
			Hash:      newHash,
			Timestamp: time.Now().UnixMilli(),
			Device:    deviceName,
		}
		
		meta.History = append(meta.History, newVersion)
		meta.Latest = newVersion
		state.Files[filePath] = meta
		saveMetadata()
		state.mu.Unlock()

		// 5. Also update the "Working Copy" for easy user browsing on server
		// This is optional but good for the "Local Lite" feel where users can see files.
		cleanPath := filepath.Join("/", filePath)
		workPath := filepath.Join(DataDir, cleanPath)
		os.MkdirAll(filepath.Dir(workPath), 0755)
		// We can just hardlink or copy. Copy is safer.
		if src, err := os.ReadFile(blobPath); err == nil {
			os.WriteFile(workPath, src, 0644)
		}

		addLog("INFO", fmt.Sprintf("File updated: %s (Hash: %s, Conflict: %v)", filePath, newHash, isConflict))

		broadcast <- map[string]interface{}{
			"type": "file_updated",
			"file": meta,
		}

		if isConflict {
			w.Header().Set("X-Conflict", "true")
			w.WriteHeader(http.StatusConflict) // 409
		} else {
			w.WriteHeader(http.StatusOK)
		}
		json.NewEncoder(w).Encode(meta)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := strings.ToLower(r.URL.Query().Get("q"))
	
	state.mu.RLock()
	defer state.mu.RUnlock()

	totalFiles := len(state.Files)
	
	results := make([]FileMetadata, 0)
	for path, meta := range state.Files {
		if query == "" || strings.Contains(strings.ToLower(path), query) {
			results = append(results, meta)
			if len(results) >= 100 { // Increased limit
				break
			}
		}
	}
	
	// Debug log if results are empty but files exist
	if len(results) == 0 && totalFiles > 0 {
		addLog("WARN", fmt.Sprintf("Search returned 0 results despite %d files existing. Query: '%s'", totalFiles, query))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state.mu.RLock()
	// Collect used hashes
	usedHashes := make(map[string]bool)
	for _, meta := range state.Files {
		usedHashes[meta.Latest.Hash] = true
		for _, v := range meta.History {
			usedHashes[v.Hash] = true
		}
	}
	state.mu.RUnlock()

	// Walk .history dir
	historyDir := filepath.Join(DataDir, ".history")
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		http.Error(w, "Failed to read history dir", http.StatusInternalServerError)
		return
	}

	deletedCount := 0
	var deletedSize int64 = 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		hash := entry.Name()
		if !usedHashes[hash] {
			info, err := entry.Info()
			if err == nil {
				if time.Since(info.ModTime()) < 10*time.Minute {
					continue // Skip young files to avoid race with upload
				}
				deletedSize += info.Size()
			}
			err = os.Remove(filepath.Join(historyDir, hash))
			if err == nil {
				deletedCount++
			} else {
				log.Printf("Failed to delete blob %s: %v", hash, err)
			}
		}
	}

	addLog("INFO", fmt.Sprintf("Cleanup: Removed %d orphaned blobs (%d bytes)", deletedCount, deletedSize))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"deletedBlobs": deletedCount,
		"freedBytes":   deletedSize,
	})
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state.mu.Lock()
	// defer state.mu.Unlock() // Removed to avoid deadlock with addLog

	// 1. Clear Memory State
	state.Files = make(map[string]FileMetadata)
	state.Logs = append(state.Logs, LogEntry{
		Timestamp: time.Now(),
		Message:   "Server reset initiated. All data cleared.",
		Level:     "WARN",
	})

	// 2. Clear Disk Data
	// Dangerous operation, ensure DataDir is correct relative path to avoid mishaps
	// We defined DataDir as "./data", so it should be safe within project.
	if err := os.RemoveAll(DataDir); err != nil {
		state.mu.Unlock() // Unlock before error return
		log.Printf("Failed to remove data dir: %v", err)
		http.Error(w, "Failed to clear data directory", http.StatusInternalServerError)
		return
	}

	// Recreate DataDir
	if err := os.MkdirAll(DataDir, 0755); err != nil {
		state.mu.Unlock() // Unlock before error return
		log.Printf("Failed to recreate data dir: %v", err)
		http.Error(w, "Failed to recreate data directory", http.StatusInternalServerError)
		return
	}

	// 3. Save Empty Metadata
	saveMetadata()

	state.mu.Unlock() // Unlock here!

	addLog("WARN", "System Reset: All files and history have been wiped.")

	// 4. Broadcast
	broadcast <- map[string]interface{}{
		"type": "server_reset",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Server reset complete"})
}

func handleBulkDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	patternsParam := r.URL.Query().Get("patterns")
	if patternsParam == "" {
		// Fallback to old 'extensions' for backward compatibility if needed, 
		// but let's just enforce 'patterns' for the new UI.
		patternsParam = r.URL.Query().Get("extensions")
	}

	if patternsParam == "" {
		http.Error(w, "Missing 'patterns' query parameter", http.StatusBadRequest)
		return
	}

	patterns := strings.Split(patternsParam, ",")
	for i, p := range patterns {
		patterns[i] = strings.TrimSpace(p)
	}

	state.mu.Lock()
	// defer state.mu.Unlock() // Removed to avoid deadlock

	deletedCount := 0
	var deletedPaths []string

	// Collect keys to delete first to avoid modifying map while iterating (though Go handles it, it's cleaner)
	toDelete := make([]string, 0)

	for path := range state.Files {
		match := false
		for _, p := range patterns {
			if p == "" { continue }

			// 1. Exact Match
			if p == path {
				match = true
				break
			}

			// 2. Suffix Match (e.g., *.jpg)
			if strings.HasPrefix(p, "*") {
				suffix := p[1:]
				// Case-insensitive suffix match? User asked for *.jpg, usually implies case insensitivity on some OSs.
				// Let's be strict unless we want to enforce lowercase everywhere. 
				// Given previous implementation was lowercase, let's do case-insensitive suffix for convenience.
				if strings.HasSuffix(strings.ToLower(path), strings.ToLower(suffix)) {
					match = true
					break
				}
			}

			// 3. Glob Match (path/filepath)
			if matched, _ := filepath.Match(p, path); matched {
				match = true
				break
			}
		}

		if match {
			toDelete = append(toDelete, path)
		}
	}

	for _, path := range toDelete {
		localPath := filepath.Join(DataDir, path)
		if err := os.Remove(localPath); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("Failed to delete file %s: %v", localPath, err)
			}
		}
		delete(state.Files, path)
		deletedPaths = append(deletedPaths, path)
		deletedCount++
	}
	
	remaining := len(state.Files)

	if deletedCount > 0 {
		saveMetadata()
	}
	state.mu.Unlock()

	addLog("INFO", fmt.Sprintf("Deleted %d files from memory map.", deletedCount))

	if deletedCount > 0 {
		addLog("INFO", fmt.Sprintf("Bulk deleted %d files matching: %s", deletedCount, patternsParam))
		
		// Broadcast deletions so clients can update
		for _, path := range deletedPaths {
			broadcast <- map[string]interface{}{
				"type": "file_deleted",
				"path": path,
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"deleted":   deletedCount,
		"paths":     deletedPaths,
		"remaining": remaining,
	})
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

// saveMetadata assumes state.mu is locked by the caller
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
            <h2>File Browser</h2>
            <input type="text" id="fileSearch" placeholder="Search files..." style="width: 100%; padding: 10px; box-sizing: border-box; margin-bottom: 10px;">
            <div style="display:flex; gap: 10px; height: 400px;">
                <div id="fileList" style="flex: 1; overflow-y: auto; border: 1px solid #eee;"></div>
                <div style="flex: 2; display: flex; flex-direction: column; gap: 10px;">
                    <div id="fileHistory" style="flex: 1; overflow-y: auto; border: 1px solid #eee; padding: 5px; display: none;">
                        <h4 style="margin: 0 0 5px 0;">History</h4>
                        <ul id="historyList" style="list-style: none; padding: 0; margin: 0;"></ul>
                    </div>
                    <div id="filePreview" style="flex: 2; border: 1px solid #eee; padding: 10px; background: #fafafa; overflow: auto; display: flex; align-items: center; justify-content: center;">
                        <p style="color: #999;">Select a file to preview</p>
                    </div>
                </div>
            </div>
        </div>

        <div class="card">
            <h2>Maintenance</h2>
            <div style="display: flex; gap: 10px;">
                <input type="text" id="deletePatterns" placeholder="*.jpg, specific/file.png" style="flex-grow: 1; padding: 10px; box-sizing: border-box;">
                <button onclick="deleteFiles()" style="padding: 10px 20px; background: #cc0000; color: white; border: none; border-radius: 4px; cursor: pointer;">Delete Files</button>
            </div>
            <p style="color: #666; font-size: 0.9em; margin-top: 5px;">Deletes files matching the patterns. Use <b>*</b> for wildcards (e.g., <b>*.png</b>).</p>
            
            <hr style="border: 0; border-top: 1px solid #eee; margin: 15px 0;">
            
            <div style="display: flex; align-items: center; justify-content: space-between;">
                <div>
                    <h3 style="margin: 0;">Clean Up History</h3>
                    <p style="color: #666; font-size: 0.9em; margin: 5px 0 0 0;">Delete version data for files that no longer exist.</p>
                </div>
                <button onclick="cleanupHistory()" style="padding: 10px 20px; background: #666; color: white; border: none; border-radius: 4px; cursor: pointer;">Clean Up</button>
            </div>

            <hr style="border: 0; border-top: 1px solid #eee; margin: 15px 0;">

            <div style="display: flex; align-items: center; justify-content: space-between;">
                <div>
                    <h3 style="margin: 0; color: #cc0000;">Danger Zone</h3>
                    <p style="color: #666; font-size: 0.9em; margin: 5px 0 0 0;">Delete ALL files and history. Start fresh.</p>
                </div>
                <button onclick="resetServer()" style="padding: 10px 20px; background: #cc0000; color: white; border: none; border-radius: 4px; cursor: pointer;">Start Fresh</button>
            </div>
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
                        '<div class="log-entry log-' + l.level + '">' + 
                        '[' + new Date(l.timestamp).toLocaleTimeString() + '] ' + l.message + 
                        '</div>'
                    ).join('');
                });
        }
        
        setInterval(fetchStatus, 2000);
        fetchStatus();

        // File Browser Logic
        const searchInput = document.getElementById('fileSearch');
        const fileList = document.getElementById('fileList');
        const filePreview = document.getElementById('filePreview');
        const fileHistory = document.getElementById('fileHistory');
        const historyList = document.getElementById('historyList');

        // Initial load
        loadFiles('');

        searchInput.addEventListener('input', (e) => {
            loadFiles(e.target.value);
        });

        function loadFiles(q) {
            fetch('/api/search?q=' + encodeURIComponent(q))
                .then(r => r.json())
                .then(files => {
                    if (files.length === 0) {
                        fileList.innerHTML = '<div style="padding:5px; color:#999;">No files found</div>';
                        return;
                    }
                    fileList.innerHTML = files.map(f => 
                        "<div class='file-item' onclick='selectFile(" + JSON.stringify(f).replace(/'/g, "\\'") + ")' style='padding: 5px; cursor: pointer; border-bottom: 1px solid #f0f0f0; font-family: monospace;'>" + 
                        f.path + 
                        "</div>"
                    ).join('');
                });
        }

        window.selectFile = function(file) {
            // Show latest
            viewFile(file.path);
            
            // Show History
            fileHistory.style.display = 'block';
            const versions = file.history || [file.latest]; 
            // Reverse to show newest first
            historyList.innerHTML = versions.slice().reverse().map(v => {
                const date = new Date(v.timestamp).toLocaleString();
                return '<li style="padding: 4px; border-bottom: 1px solid #eee; cursor: pointer;" onclick="viewBlob(\'' + v.hash + '\', \'' + file.path.replace(/'/g, "\\'") + '\')">' +
                    '<b>' + date + '</b><br>' +
                    '<span style="font-size: 0.8em; color: #666;">' + v.device + ' (' + v.hash.substr(0,7) + ')</span>' +
                '</li>';
            }).join('');
        }

        window.viewBlob = function(hash, path) {
             // We don't have a direct blob API yet, but we can rely on the file structure if we exposed it?
             // Wait, I implemented serving from .history/{hash} if GET is used? 
             // No, the GET /api/file?path=... serves latest.
             // I should add support for ?version=hash to /api/file
             // BUT, for now, let's just assume viewing the file by path views the LATEST.
             // To view OLD versions, I need to update the GET handler. 
             // Let's stick to "View Latest" for now as requested, OR hack it.
             // Actually, I can map /api/file?path=.history/{hash} if I allowed it.
             // But handleFileOperations prevents traversal. 
             alert("Viewing historical versions is not yet fully implemented in API. Viewing latest.");
             viewFile(path);
        }

        window.viewFile = function(path) {
            const url = '/api/file?path=' + encodeURIComponent(path);
            const ext = path.split('.').pop().toLowerCase();
            const isImage = ['jpg', 'jpeg', 'png', 'gif', 'webp', 'svg'].includes(ext);
            
            filePreview.innerHTML = '<p>Loading...</p>';

            if (isImage) {
                filePreview.innerHTML = '<img src="' + url + '" style="max-width: 100%; max-height: 100%; display: block; margin: auto;">';
            } else {
                fetch(url)
                    .then(r => r.text())
                    .then(text => {
                        filePreview.innerHTML = '<pre style="white-space: pre-wrap; word-break: break-all; text-align: left; margin: 0;">' + text.replace(/</g, '&lt;') + '</pre>';
                    })
                    .catch(err => {
                        filePreview.innerHTML = '<p style="color:red">Error loading file</p>';
                    });
            }
        }

        function deleteFiles() {
            const patterns = document.getElementById('deletePatterns').value;
            if (!patterns) {
                alert('Please enter patterns to delete');
                return;
            }
            if (!confirm('Are you sure you want to delete files matching: ' + patterns + '? This cannot be undone.')) {
                return;
            }

            fetch('/api/files/delete?patterns=' + encodeURIComponent(patterns), { method: 'POST' })
                .then(r => r.json())
                .then(data => {
                    alert('Deleted ' + data.deleted + ' files.');
                    fetchStatus(); 
                    loadFiles(document.getElementById('fileSearch').value || ''); 
                })
                .catch(err => alert('Error: ' + err));
        }

        function cleanupHistory() {
            if (!confirm('Are you sure you want to clean up orphaned version history? This will delete data for files that have been deleted.')) {
                return;
            }
            fetch('/api/cleanup', { method: 'POST' })
                .then(r => r.json())
                .then(data => {
                    alert('Cleanup complete.\nDeleted Blobs: ' + data.deletedBlobs + '\nFreed Bytes: ' + data.freedBytes);
                })
                .catch(err => alert('Error: ' + err));
        }

        function resetServer() {
            if (!confirm('WARNING: This will delete ALL files and history from the server. This action cannot be undone. Are you sure?')) {
                return;
            }
            if (!confirm('Double check: You are about to WIPE EVERYTHING. Proceed?')) {
                return;
            }
            
            fetch('/api/reset', { method: 'POST' })
                .then(r => {
                    if (!r.ok) {
                        return r.text().then(text => { throw new Error(text || r.statusText) });
                    }
                    return r.json();
                })
                .then(data => {
                    alert('Server reset complete.');
                    window.location.reload();
                })
                .catch(err => alert('Error: ' + err));
        }
    </script>
</body>
</html>
`
