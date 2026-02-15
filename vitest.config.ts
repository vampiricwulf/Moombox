import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    globals: true,
    environment: "node",
    coverage: {
      provider: "v8",
      reporter: ["text", "json", "html"],
      include: ["src/**/*.ts"],
      exclude: [
        "dist/**",
        "src/web/public/**",
        "**/*.d.ts",
        "scripts/**",
        "**/*.test.ts",
        "**/*.spec.ts",
        // Exclude SEA build artifacts
        "src/bgutils/**", // External port, complex to test
        "src/cipher/**", // External port, complex to test
        "src/ejs/**", // External port, complex to test
      ],
    },
  },
});
