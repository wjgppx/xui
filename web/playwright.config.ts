import { defineConfig } from '@playwright/test'
export default defineConfig({ testDir: './tests', timeout: 90_000, workers: 1, use: { baseURL: 'http://127.0.0.1:5173', screenshot: 'only-on-failure', trace: 'retain-on-failure', launchOptions: { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined } } })
