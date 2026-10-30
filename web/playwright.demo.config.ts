import { defineConfig } from '@playwright/test'

export default defineConfig({
 testDir: './demo-tests', timeout: 30_000, workers: 1,
 use: { baseURL: process.env.XUI_DEMO_BASE_URL || `http://127.0.0.1:5174${process.env.XUI_DEMO_BASE_PATH || '/xui/'}`, screenshot: 'only-on-failure', trace: 'retain-on-failure', launchOptions: { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined } },
})
