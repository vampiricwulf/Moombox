export function extractPlayerId(playerUrl: string): string {
    try {
        const url = new URL(playerUrl);
        const pathParts = url.pathname.split('/');
        const playerIndex = pathParts.indexOf('player');
        if (playerIndex !== -1 && playerIndex + 1 < pathParts.length) {
            return pathParts[playerIndex + 1];
        }
    } catch (e) {
        // Fallback for relative paths
        const match = playerUrl.match(/\/s\/player\/([^\/]+)/);
        if (match) {
            return match[1];
        }
    }
    return 'unknown';
}