import { defineConfig, loadEnv } from "vite";
import solid from "vite-plugin-solid";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, ".", "VIRE_");
  const gatewayPort = Number(env.VIRE_GATEWAY_PORT || 8080);
  const webPort = Number(env.VIRE_WEB_PORT || 5173);
  const gateway = `http://127.0.0.1:${gatewayPort}`;

  return {
    plugins: [solid()],
    server: {
      host: "127.0.0.1",
      port: webPort,
      strictPort: true,
      proxy: {
        "/api": gateway,
        "/v1": gateway,
      },
    },
  };
});
