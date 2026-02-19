import ms from "ms";
import { ConfigManager } from './config.js';
import { Logger } from './logger.js';
import { fetchWithTimeout } from './http.js';
import { getErrorMessage } from "../types/errors.js";

export enum NotificationType {
    INFO = 0x3498db,       // Blue — found, added, scheduled
    SUCCESS = 0x2ecc71,    // Green — live, finished
    WARNING = 0xf1c40f,    // Yellow — auth required
    ERROR = 0xe74c3c,      // Red — error
    DOWNLOAD = 0x1abc9c,   // Teal — download starting
    MUXING = 0x9b59b6,     // Purple — muxing
    CANCELLED = 0xe67e22,  // Orange — cancelled
}

export interface NotificationOptions {
    url?: string;           // Makes embed title clickable
    thumbnail?: string;     // Small right-aligned image URL
    image?: string;         // Large full-width image URL
    event?: string;         // Event name for filtering (e.g. "found", "finished")
}

export class NotificationManager {
    private static instance: NotificationManager;
    private logger: Logger;

    private constructor() {
        this.logger = Logger.getInstance();
    }

    static getInstance(): NotificationManager {
        if (!NotificationManager.instance) {
            NotificationManager.instance = new NotificationManager();
        }
        return NotificationManager.instance;
    }

    async send(title: string, message: string, type: NotificationType = NotificationType.INFO, fields?: { name: string, value: string, inline?: boolean }[], options?: NotificationOptions) {
        const config = ConfigManager.getInstance().get();
        if (!config.notifications || config.notifications.length === 0) {
            return;
        }

        for (const notifConfig of config.notifications) {
            if (!notifConfig.url) continue;

            // Event filtering: if events array is configured, only send matching events
            if (options?.event && notifConfig.events?.length) {
                if (!notifConfig.events.includes(options.event)) continue;
            }

            // Validate and route webhook URLs
            if (notifConfig.url.startsWith('discord://')) {
                // Apprise-style discord:// URL
                await this.sendDiscord(notifConfig.url, title, message, type, fields, options);
            } else if (/^https:\/\/(?:\w+\.)?discord\.com\/api\/webhooks\/\d+\/[\w-]+/.test(notifConfig.url)) {
                // Standard Discord webhook URL (https only, validated pattern)
                await this.sendDiscord(notifConfig.url, title, message, type, fields, options);
            } else if (notifConfig.url.includes('discord.com/api/webhooks')) {
                this.logger.warn(`[Notifications] Rejected webhook URL (must use https and match Discord webhook pattern): ${notifConfig.url}`);
            } else {
                this.logger.warn(`[Notifications] Unsupported notification URL format: ${notifConfig.url}`);
            }
        }
    }

    private async sendDiscord(url: string, title: string, message: string, color: number, fields?: { name: string, value: string, inline?: boolean }[], options?: NotificationOptions) {
        let actualUrl = url;
        if (url.startsWith('discord://')) {
            // Convert apprise-style discord://ID/TOKEN to https://discord.com/api/webhooks/ID/TOKEN
            const parts = url.replace('discord://', '').split('/');
            if (parts.length >= 2) {
                actualUrl = `https://discord.com/api/webhooks/${parts[0]}/${parts[1]}`;
            }
        }

        const payload = {
            embeds: [{
                title: title,
                description: message,
                color: color,
                url: options?.url,
                fields: fields || [],
                thumbnail: options?.thumbnail ? { url: options.thumbnail } : undefined,
                image: options?.image ? { url: options.image } : undefined,
                footer: {
                    text: 'Moombox Node.js'
                },
                timestamp: new Date().toISOString()
            }]
        };

        try {
            const resp = await fetchWithTimeout(actualUrl, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            }, ms("15s"));

            if (!resp.ok) {
                this.logger.error(`[Notifications] Failed to send Discord webhook: ${resp.status} ${resp.statusText}`);
            }
        } catch (e: unknown) {
            this.logger.error(`[Notifications] Error sending Discord webhook: ${getErrorMessage(e)}`);
        }
    }
}
