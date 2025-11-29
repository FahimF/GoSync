# Local Sync Lite

A lightweight, self-hosted synchronization solution for Obsidian, designed for local networks.

## Components

1.  **Server (`/server`)**: A single-binary Go server that acts as the central hub.
2.  **Plugin (`/plugin`)**: An Obsidian plugin that talks to the hub.

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
3.  Run the server:
    ```bash
    go run main.go
    ```
    *The server will start at `http://localhost:8080` and store data in `./data`.*

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
2.  Ensure the **Server URL** matches your Go server (e.g., `http://localhost:8080` or `http://192.168.1.X:8080` if running on another machine).
3.  Click "Sync Now" or start editing files!

## Architecture

*   **Protocol**: HTTP (GET/PUT) for file transfer, WebSocket for real-time notifications.
*   **Conflict Resolution**: Simple "Last Write Wins" (Server overrides Client on conflict during download).
*   **Storage**: Files are stored as plain text in the `server/data` folder.
