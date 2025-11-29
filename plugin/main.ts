import { App, Plugin, PluginSettingTab, Setting, TFile, Notice, requestUrl, normalizePath } from 'obsidian';

interface LocalSyncSettings {
	serverUrl: string;
	deviceName: string;
}

const DEFAULT_SETTINGS: LocalSyncSettings = {
	serverUrl: 'http://localhost:8080',
	deviceName: 'Obsidian Client'
}

interface RemoteFile {
	path: string;
	hash: string;
	updatedAt: number;
}

export default class LocalSyncLite extends Plugin {
	settings: LocalSyncSettings;
	ws: WebSocket | null = null;
	syncQueue: Set<string> = new Set();
	isSyncing = false;

	async onload() {
		await this.loadSettings();

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
		this.connectWebSocket();
		this.startSync();
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
		this.connectWebSocket();
	}

	connectWebSocket() {
		if (this.ws) {
			this.ws.close();
		}

		const wsUrl = this.settings.serverUrl.replace('http', 'ws') + '/ws';
		try {
			this.ws = new WebSocket(wsUrl);

			this.ws.onopen = () => {
				console.log('Connected to LocalSync Server');
				new Notice('Connected to Sync Server');
				
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
					}
				} catch (e) {
					console.error('Error parsing WS message', e);
				}
			};

			this.ws.onclose = () => {
				console.log('Disconnected from Sync Server');
				// Reconnect logic could go here
				setTimeout(() => this.connectWebSocket(), 5000);
			};
		} catch (e) {
			console.error('Failed to connect to WebSocket', e);
		}
	}

	async startSync() {
		new Notice('Starting Full Sync...');
		try {
			// 1. Get Remote Files
			const response = await requestUrl({
				url: `${this.settings.serverUrl}/api/files`,
				method: 'GET'
			});
			
			const remoteFiles: RemoteFile[] = response.json;
			const localFiles = this.app.vault.getFiles();
			
			console.log(`Sync started. Remote files: ${remoteFiles.length}, Local files: ${localFiles.length}`);

			// 2. Compare and Sync
			for (const remote of remoteFiles) {
				const local = this.app.vault.getAbstractFileByPath(remote.path);
				if (local instanceof TFile) {
					const localHash = await this.calculateFileHash(local);
					if (localHash !== remote.hash) {
						// Conflict! For "Lite", we assume Server Wins if remote is newer? 
						// Or just download. Let's download for now.
						await this.downloadFile(remote);
					}
				} else {
					// Local doesn't exist, download
					await this.downloadFile(remote);
				}
			}

			// 3. Upload missing local files (optional for full sync, or wait for lazy sync)
			for (const local of localFiles) {
				// Check if exists on remote
				const remote = remoteFiles.find(r => r.path === local.path);
				if (!remote) {
					console.log(`File missing on remote: ${local.path}, uploading...`);
					await this.uploadFile(local);
				}
			}
			new Notice('Sync Complete');

		} catch (e) {
			console.error('Sync Error:', e);
			new Notice('Sync Failed: ' + e);
		}
	}

	async downloadFile(remote: RemoteFile) {
		console.log(`Downloading ${remote.path}...`);
		try {
			const response = await requestUrl({
				url: `${this.settings.serverUrl}/api/file?path=${encodeURIComponent(remote.path)}`,
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
		} catch (e) {
			console.error(`Failed to download ${remote.path}`, e);
		}
	}

	async uploadFile(file: TFile) {
        if (this.isSyncing) return; // Simple lock

		console.log(`Uploading ${file.path}...`);
		try {
			const content = await this.app.vault.readBinary(file);
			
			await requestUrl({
				url: `${this.settings.serverUrl}/api/file?path=${encodeURIComponent(file.path)}`,
				method: 'PUT',
				body: content,
				headers: {
					'Content-Type': 'application/octet-stream'
				}
			});
            
            console.log(`Uploaded ${file.path}`);
		} catch (e) {
			console.error(`Failed to upload ${file.path}`, e);
		}
	}

	async handleLocalChange(file: TFile) {
        // Debounce could go here
        await this.uploadFile(file);
	}

	async handleRemoteUpdate(remote: RemoteFile) {
		const local = this.app.vault.getAbstractFileByPath(remote.path);
        if (local instanceof TFile) {
            const hash = await this.calculateFileHash(local);
            if (hash !== remote.hash) {
                await this.downloadFile(remote);
                new Notice(`Updated: ${remote.path}`);
            }
        } else {
            await this.downloadFile(remote);
            new Notice(`Created: ${remote.path}`);
        }
	}

	async calculateFileHash(file: TFile): Promise<string> {
		const content = await this.app.vault.read(file);
		const msgBuffer = new TextEncoder().encode(content);
		const hashBuffer = await crypto.subtle.digest('SHA-256', msgBuffer);
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
