import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig(({ mode }) => {
  const { NASTOOL_GO_ORIGIN } = loadEnv(mode, ".", "NASTOOL_GO_ORIGIN");
  const target = new URL(NASTOOL_GO_ORIGIN || "http://127.0.0.1:3001");
  const httpOrigin = target.protocol === "http:" || target.protocol === "https:";
  if (!httpOrigin || target.username || target.password || target.pathname !== "/" || target.search || target.hash) {
    throw new Error("NASTOOL_GO_ORIGIN must be an HTTP(S) origin without credentials, path, query or fragment");
  }

  return {
    plugins: [react()],
    server: {
      host: "0.0.0.0",
      port: 5173,
      proxy: {
        "^/(?:api(?:[/?]|$)|do(?:[?]|$)|stream-progress(?:[?]|$))": {
          target: target.origin,
          changeOrigin: true,
        },
      },
    },
  };
});
