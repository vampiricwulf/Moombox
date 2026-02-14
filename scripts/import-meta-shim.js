// Shim for import.meta.url in CJS context
export const importMetaUrl = require('url').pathToFileURL(__filename).href;
