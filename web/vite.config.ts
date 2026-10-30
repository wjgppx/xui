import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, '.', 'VITE_')
  const base = env.VITE_BASE_PATH || '/'
  if (base !== '/' && (!/^\/[A-Za-z0-9._-]+(?:\/[A-Za-z0-9._-]+)*\/$/.test(base) || base.split('/').some(part => part === '.' || part === '..'))) {
    throw new Error('VITE_BASE_PATH must be / or a slash-delimited local path ending in /')
  }
  return {
    base,
    plugins: [vue()],
    server: { strictPort: true, proxy: { '/api': 'http://127.0.0.1:8080', '/healthz': 'http://127.0.0.1:8080' } },
  }
})
