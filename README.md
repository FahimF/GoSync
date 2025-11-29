# Local Sync Lite

A lightweight, self-hosted synchronization solution for Obsidian, designed for local networks.

## Features

*   **File Versioning & History**: The server maintains a history of file changes, allowing you to browse past versions directly from the web interface.
*   **Conflict Detection**: Server-side detection of concurrent modifications using base hashes, notifying the client of potential conflicts.
*   **Client-Side Sync Control**: Toggle synchronization on/off and trigger manual resyncs from the plugin settings.
*   **Real-time Sync**: Uses WebSockets for instant notifications of changes.
*   **Web Dashboard**: Monitor connected devices, server logs, and file stats via a built-in web interface.
*   **Binary Support**: Correctly syncs images, PDFs, and other binary assets alongside Markdown files.
*   **Simple Architecture**: Single binary server, standard Obsidian plugin.

## Components

1.  **Server (`/server`)**: A single-binary Go server that acts as the central hub and dashboard. It stores the latest file versions in `./data` and all historical versions (as immutable blobs) in `./data/.history`.
2.  **Plugin (`/plugin`)**: An Obsidian plugin that connects to the hub.

## Setup Instructions

### 1. Start the Server

Prerequisites: [Go](https://go.dev/) installed.

1.  Navigate to the server directory:
    ```bash
    cd server
    ```
2.  Install dependencies:
    ```bash
    go mod tidy
    ```
3.  Run the server (Dev mode):
    ```bash
    go run main.go
    ```
    *The server will start at `http://localhost:8080`. Data is stored in `./data`.*

4.  **Dashboard**: Open `http://localhost:8080` in your browser to view logs and connected devices.

### 2. Install the Plugin

Prerequisites: [Node.js](https://nodejs.org/) installed.

1.  Navigate to the plugin directory:
    ```bash
    cd plugin
    ```
2.  Install dependencies:
    ```bash
    npm install
    ```
3.  Build the plugin:
    ```bash
    npm run build
    ```
4.  **Install into Obsidian**:
    *   Create a folder named `obsidian-local-sync-lite` inside your Obsidian Vault's `.obsidian/plugins/` directory.
    *   Copy `main.js`, `manifest.json`, and `styles.css` (if any) to that folder.
    *   Enable the plugin in Obsidian Settings > Community Plugins.

### 3. Configure

1.  Open Obsidian Settings > Local Sync Lite.
2.  **Server URL**: Set to your server's address (e.g., `http://localhost:8080` or `http://192.168.1.X:8080` if running on another machine).
3.  **Device Name**: Give your device a friendly name (e.g., "My MacBook") to identify it in the server dashboard and in file history.
4.  **Enable Sync**: Toggle this ON to activate synchronization. (It is disabled by default).
5.  **Resync All**: Use the "Resync" button to force a full re-synchronization of all files.

## Building & Deployment

### Creating a Server Binary

To create a standalone executable for deployment (no Go installation required on the target machine):

1.  Navigate to the server directory:
    ```bash
    cd server
    ```
2.  Build the binary:
    *   **macOS/Linux**:
        ```bash
        go build -o local-obsidian-sync main.go
        ```
    *   **Windows**:
        ```bash
        go build -o local-obsidian-sync.exe main.go
        ```
    *   **Cross-compilation** (e.g., building for Linux on macOS):
        ```bash
        GOOS=linux GOARCH=amd64 go build -o local-obsidian-sync main.go
        ```

### Deploying the Server

1.  **Transfer**: Copy the generated binary (`local-obsidian-sync` or `.exe`) to your target machine (e.g., a Raspberry Pi, NAS, or always-on PC).
2.  **Directory Setup**: The server creates a `./data` directory and a `metadata.json` file in its working directory. Ensure the user running the binary has write permissions to the folder.
3.  **Run**:
    ```bash
    ./local-obsidian-sync
    ```
4.  **Background Usage**: To keep it running in the background, you can use tools like `nohup`, `screen`, or create a systemd service (on Linux).
    *   **Simple Background Run**:
        ```bash
        nohup ./local-obsidian-sync > server.log 2>&1 &
        ```

## Architecture

*   **Protocol**: HTTP (GET/PUT) for file transfer, WebSocket for real-time notifications.
*   **Conflict Resolution**: Server-side detection of conflicts (when a client attempts to update a file based on an outdated version) and notification to the client. The client is responsible for further action.
*   **Storage**: Files are stored in a versioned manner. The latest version resides in the `server/data` folder (as a working copy), and all historical versions are stored as immutable blobs in `server/data/.history/{hash}`.