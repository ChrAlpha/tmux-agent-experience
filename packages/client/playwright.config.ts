import { defineConfig, devices } from '@playwright/test'
import { origin } from './e2e/world'

// The page on a phone's screen, in Safari's engine and in Chrome's, against
// the world e2e/world.ts builds. The projects run one after the other: they
// share the world's agents.
export default defineConfig({
  testDir: 'e2e',
  globalSetup: './e2e/world.ts',
  workers: 1,
  timeout: 45_000,
  expect: { timeout: 10_000 },
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [['github'], ['list']] : 'list',
  use: { baseURL: origin, trace: 'retain-on-failure' },
  projects: [
    { name: 'iphone', use: { ...devices['iPhone 15'] } },
    { name: 'android', use: { ...devices['Pixel 7'] } },
  ],
})
