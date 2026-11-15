import { defineConfig, mergeConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { tokenrouterPlugin } from '@tokenflux/tokenrouter-plugin-sdk/vite'

// vite.config.ts builds the intelligence plugin using its declared installation
// identity for frontend registration and scoped styles. The host derives page
// addresses from that same manifest identity, so the build identifier stays in
// sync with plugin.json for installation and navigation.

export default defineConfig(mergeConfig(tokenrouterPlugin({ pluginId: 'moonup.intelligence', outDir: '../dist/frontend' }), { plugins: [vue()] }))
