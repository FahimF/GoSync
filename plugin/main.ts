import { App, Plugin, PluginSettingTab, Setting, TFile, Notice, requestUrl, normalizePath } from 'obsidian';

interface LocalSyncSettings {
	serverUrl: string;
	deviceName: string;
    enableSync: boolean;
}

const DEFAULT_SETTINGS: LocalSyncSettings = {
	serverUrl: 'http://localhost:8080',
	deviceName: 'Obsidian Client',
    enableSync: false
}

interface RemoteFile {
	path: string;
	hash: string;
	updatedAt: number;
    latest?: {
        hash: string;
        timestamp: number;
    }
}

interface LocalState {
    [path: string]: string; // Path -> Last Synced Hash
}

export default class LocalSyncLite extends Plugin {
	settings: LocalSyncSettings;
    localState: LocalState = {};
	ws: WebSocket | null = null;
	syncQueue: Set<string> = new Set();
	isSyncing = false;

	async onload() {
		await this.loadSettings();
        const data = await this.loadData();
        this.localState = Object.assign({}, data?.localState || {});

		// Add settings tab
		this.addSettingTab(new LocalSyncSettingTab(this.app, this));

		// Status Bar
		const statusBarItemEl = this.addStatusBarItem();
		statusBarItemEl.setText('LocalSync: Idle');

		// Initial Sync Command
		this.addCommand({
			id: 'sync-now',
			name: 'Sync Now',
			callback: () => {
				this.startSync();
			}
		});

		// Watch for changes
		this.registerEvent(this.app.vault.on('modify', (file) => {
			if (file instanceof TFile) {
				this.handleLocalChange(file);
			}
		}));
		
		this.registerEvent(this.app.vault.on('create', (file) => {
			if (file instanceof TFile) {
				this.handleLocalChange(file);
			}
		}));

		// Connect on load
        if (this.settings.enableSync) {
		    this.connectWebSocket();
            // Wait a bit for vault to be ready?
            // actually onload is called when vault is ready.
		    this.startSync();
        }
	}

	async onunload() {
		if (this.ws) {
			this.ws.close();
		}
	}

	async loadSettings() {
		this.settings = Object.assign({}, DEFAULT_SETTINGS, await this.loadData());
	}

	async saveSettings() {
		await this.saveData(this.settings);
		// Reconnect if settings changed
        if (this.settings.enableSync) {
		    this.connectWebSocket();
        } else {
            if (this.ws) {
                this.ws.close();
                this.ws = null;
            }
        }
	}

    // Unified Logging Helper
    async log(message: string, level: 'INFO' | 'WARN' | 'ERROR' = 'INFO', showNotice = false) {
        const timestamp = new Date().toLocaleTimeString();
        const logMsg = `[${timestamp}] [${level}] ${message}`;

        // 1. Console
        if (level === 'ERROR') console.error(logMsg);
        else if (level === 'WARN') console.warn(logMsg);
        else console.log(logMsg);

        // 2. Notice (Optional)
        if (showNotice) {
            new Notice(message);
        }

        // 3. Server (Remote Log)
        if (this.ws && this.ws.readyState === WebSocket.OPEN) {
            this.ws.send(JSON.stringify({
                type: 'client_log',
                level: level,
                message: message
            }));
        }

        // 4. Permanent File Log (GoSync Logs.md)
        // Append to file. 
        try {
            const logPath = 'GoSync Logs.md';
            const adapter = this.app.vault.adapter;
            if (await adapter.exists(logPath)) {
                await adapter.append(logPath, logMsg + '\n');
            } else {
                await adapter.write(logPath, '# GoSync Client Logs\n\n' + logMsg + '\n');
            }
        } catch (e) {
            console.error("Failed to write to log file", e);
        }
    }

	connectWebSocket() {
		if (this.ws) {
			this.ws.close();
		}

		const wsUrl = this.settings.serverUrl.replace('http', 'ws') + '/ws';
		try {
			this.ws = new WebSocket(wsUrl);

			this.ws.onopen = () => {
				this.log('Connected to LocalSync Server', 'INFO', true);
				
				// Send Identification
				if (this.ws && this.ws.readyState === WebSocket.OPEN) {
					this.ws.send(JSON.stringify({
						type: 'identify',
						deviceName: this.settings.deviceName
					}));
				}
				
				// Trigger sync on connect/reconnect
				this.startSync();
			};

			this.ws.onmessage = async (event) => {
				try {
					const msg = JSON.parse(event.data);
					if (msg.type === 'file_updated') {
						await this.handleRemoteUpdate(msg.file);
					} else if (msg.type === 'server_reset') {
                        this.log('Server was reset. Resyncing...', 'WARN', true);
                        this.localState = {};
                        await this.saveData({ ...this.settings, localState: this.localState });
                        this.startSync();
                    }
				} catch (e) {
					console.error('Error parsing WS message', e);
				}
			};

			this.ws.onclose = () => {
				this.log('Disconnected from Sync Server', 'WARN');
				// Reconnect logic could go here
				setTimeout(() => this.connectWebSocket(), 5000);
			};
		} catch (e) {
			this.log('Failed to connect to WebSocket: ' + e, 'ERROR');
		}
	}

	async startSync() {
        if (this.isSyncing) return;
        this.isSyncing = true;
		this.log('Starting Full Sync...', 'INFO', true);
        
        let uploaded = 0;
        let downloaded = 0;

		try {
			// 1. Get Remote Files
			const response = await requestUrl({
				url: `${this.settings.serverUrl}/api/files`,
				method: 'GET'
			});
			
			const remoteFiles: RemoteFile[] = response.json;
			const localFiles = this.app.vault.getFiles();
			
            // DEBUG: Verify vault access
            this.log(`Vault Debug: Name='${this.app.vault.getName()}', FilesFound=${localFiles.length}`, 'INFO');
            if (localFiles.length > 0) {
                 this.log(`First local file: ${localFiles[0].path}`, 'INFO');
            }

            const msg = `Sync analysis: Server has ${remoteFiles.length} files, Local has ${localFiles.length} files.`;
            this.log(msg, 'INFO', true);

			// 2. Compare and Sync
			for (const remote of remoteFiles) {
                // Normalize remote structure
                const remoteHash = remote.latest ? remote.latest.hash : remote.hash;

				const local = this.app.vault.getAbstractFileByPath(remote.path);
				if (local instanceof TFile) {
					const localHash = await this.calculateFileHash(local);
					if (localHash !== remoteHash) {
                        const baseHash = this.localState[remote.path];
                        if (baseHash && baseHash !== remoteHash && baseHash !== localHash) {
                             this.log(`Possible conflict for ${remote.path}`, 'WARN');
                        }
						if (await this.downloadFile({ ...remote, hash: remoteHash }, true)) {
                            downloaded++;
                        }
					}
				} else {
					// Local doesn't exist, download
					if (await this.downloadFile({ ...remote, hash: remoteHash }, true)) {
                        downloaded++;
                    }
				}
			}

            // 2.5 Clean up Local State
            for (const localPath in this.localState) {
                const remote = remoteFiles.find(r => r.path === localPath);
                if (!remote) {
                    delete this.localState[localPath];
                }
            }
            this.saveData({ ...this.settings, localState: this.localState });

			// 3. Upload missing local files
			for (const local of localFiles) {
                // Ignore the log file itself to prevent infinite loops
                if (local.path === 'GoSync Logs.md') continue;

				// Check if exists on remote
				const remote = remoteFiles.find(r => r.path === local.path);
				if (!remote) {
					this.log(`File missing on remote: ${local.path}, uploading...`, 'INFO');
					if (await this.uploadFile(local, true)) {
                        uploaded++;
                    }
				}
			}
			this.log(`Sync Complete. Uploaded: ${uploaded}, Downloaded: ${downloaded}`, 'INFO', true);

		} catch (e) {
			this.log('Sync Error: ' + e, 'ERROR', true);
		} finally {
            this.isSyncing = false;
        }
	}

	async downloadFile(remote: RemoteFile, skipLock = false): Promise<boolean> {
        if (!skipLock && this.isSyncing) return false;

		this.log(`Downloading ${remote.path}...`, 'INFO');
		try {
			const response = await requestUrl({
				url: `${this.settings.serverUrl}/api/file?path=${this.fixedEncodeURIComponent(remote.path)}`,
				method: 'GET'
			});

			// Use arrayBuffer to handle both text and binary files correctly
			const content = response.arrayBuffer;
            const file = this.app.vault.getAbstractFileByPath(remote.path);

            if (file instanceof TFile) {
                await this.app.vault.modifyBinary(file, content);
            } else {
				// Ensure folder exists
				const folderPath = remote.path.substring(0, remote.path.lastIndexOf('/'));
				if (folderPath && !this.app.vault.getAbstractFileByPath(folderPath)) {
					await this.app.vault.createFolder(folderPath);
				}
                await this.app.vault.createBinary(remote.path, content);
            }
            
            // Update local state
            this.localState[remote.path] = remote.hash; // Assuming remote.hash is the hash of the downloaded content
            this.saveData({ ...this.settings, localState: this.localState });
            return true;

		} catch (e) {
			this.log(`Failed to download ${remote.path}: ${e}`, 'ERROR');
            return false;
		}
	}

	async uploadFile(file: TFile, skipLock = false): Promise<boolean> {
        if (!skipLock && this.isSyncing) return false; // Simple lock

		this.log(`Uploading ${file.path}...`, 'INFO');
		try {
			const content = await this.app.vault.readBinary(file);
            const baseHash = this.localState[file.path] || "";

            // Use requestUrl instead of fetch to avoid CORS/Network issues
            const response = await requestUrl({
                url: `${this.settings.serverUrl}/api/file?path=${this.fixedEncodeURIComponent(file.path)}`,
                method: 'PUT',
                body: content,
                headers: {
                    'Content-Type': 'application/octet-stream',
                    'X-Device-Name': this.settings.deviceName,
                    'X-Base-Hash': baseHash
                },
                throw: false // Handle errors manually
            });

			if (response.status < 200 || response.status >= 300) {
                if (response.status === 409) {
                    this.log(`Conflict detected for ${file.path}! Server has a newer version.`, 'WARN', true);
                    return false;
                }
				throw new Error(`Server responded with ${response.status}`);
			}
            
            // Update local state with the new hash returned by server
            const result = response.json;
            if (result && result.latest && result.latest.hash) {
                this.localState[file.path] = result.latest.hash;
                this.saveData({ ...this.settings, localState: this.localState });
            }

            this.log(`Uploaded ${file.path}`, 'INFO');
            return true;
		} catch (e) {
			this.log(`Failed to upload ${file.path}: ${e}`, 'ERROR');
            return false;
		}
	}

    // Helper to strictly encode URI components (including !, ', (, ), *)
    fixedEncodeURIComponent(str: string) {
        return encodeURIComponent(str).replace(/[!'()*]/g, function(c) {
            return '%' + c.charCodeAt(0).toString(16);
        });
    }

	async handleLocalChange(file: TFile) {
        if (!this.settings.enableSync) return;
        if (file.path === 'GoSync Logs.md') return; // Don't sync logs
        // Debounce could go here
        await this.uploadFile(file);
	}

	async handleRemoteUpdate(remote: RemoteFile) {
        if (!this.settings.enableSync) return;
        const remoteHash = remote.latest ? remote.latest.hash : remote.hash;
		const local = this.app.vault.getAbstractFileByPath(remote.path);
        if (local instanceof TFile) {
            const hash = await this.calculateFileHash(local);
            if (hash !== remoteHash) {
                await this.downloadFile({ ...remote, hash: remoteHash });
                this.log(`Updated: ${remote.path}`, 'INFO', true);
            }
        } else {
            await this.downloadFile({ ...remote, hash: remoteHash });
            this.log(`Created: ${remote.path}`, 'INFO', true);
        }
	}

	async calculateFileHash(file: TFile): Promise<string> {
		const content = await this.app.vault.readBinary(file);
		const hashBuffer = await crypto.subtle.digest('SHA-256', content);
		const hashArray = Array.from(new Uint8Array(hashBuffer));
		return hashArray.map(b => b.toString(16).padStart(2, '0')).join('');
	}
}

class LocalSyncSettingTab extends PluginSettingTab {
	plugin: LocalSyncLite;

	constructor(app: App, plugin: LocalSyncLite) {
		super(app, plugin);
		this.plugin = plugin;
	}

	display(): void {
		const {containerEl} = this;

		containerEl.empty();

		new Setting(containerEl)
			.setName('Enable Sync')
			.setDesc('Enable synchronization with the server')
			.addToggle(toggle => toggle
				.setValue(this.plugin.settings.enableSync)
				.onChange(async (value) => {
					this.plugin.settings.enableSync = value;
					await this.plugin.saveSettings();
                    if (value) {
                        this.plugin.startSync();
                    }
				}));

        new Setting(containerEl)
            .setName('Resync All')
            .setDesc('Force a full resync with the server')
            .addButton(button => button
                .setButtonText('Resync')
                .onClick(async () => {
                    if (this.plugin.settings.enableSync) {
                        await this.plugin.startSync();
                    } else {
                        new Notice('Sync is disabled. Please enable it first.');
                    }
                }));

		new Setting(containerEl)
			.setName('Server URL')
			.setDesc('The URL of your local GoSync server (e.g., http://localhost:8080)')
			.addText(text => text
				.setPlaceholder('http://localhost:8080')
				.setValue(this.plugin.settings.serverUrl)
				.onChange(async (value) => {
					this.plugin.settings.serverUrl = value;
					await this.plugin.saveSettings();
				}));

		new Setting(containerEl)
			.setName('Device Name')
			.setDesc('Name to identify this device')
			.addText(text => text
				.setPlaceholder('My Mac')
				.setValue(this.plugin.settings.deviceName)
				.onChange(async (value) => {
					this.plugin.settings.deviceName = value;
					await this.plugin.saveSettings();
				}));
	}
}
