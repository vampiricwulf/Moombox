import fs from 'fs-extra';
import { getPlayerFilePath } from "../playerCache.js";
import type { StsRequest, StsResponse } from "../types.js";
import { stsCache } from "../stsCache.js";

export async function getSts(request: StsRequest): Promise<StsResponse> {
    const { player_url } = request;
    const playerFilePath = await getPlayerFilePath(player_url);

    const cachedSts = stsCache.get(playerFilePath);
    if (cachedSts) {
        return { sts: cachedSts };
    }

    const playerContent = await fs.readFile(playerFilePath, 'utf-8');

    const stsPattern = /(signatureTimestamp|sts):(\d+)/;
    const match = playerContent.match(stsPattern);

    if (match && match[2]) {
        const sts = match[2];
        stsCache.set(playerFilePath, sts);
        return { sts };
    } else {
        throw new Error("Timestamp not found in player script");
    }
}