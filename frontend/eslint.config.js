import js from "@eslint/js";
import { defineConfig } from "eslint/config";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

export default defineConfig(
  {
    ignores: ["dist", "coverage", "playwright-report", "test-results"],
  },
  js.configs.recommended,
  // Build scripts and their node:test suites run in Node.
  {
    files: ["scripts/**/*.mjs", "*.config.js"],
    languageOptions: { globals: globals.node },
  },
  // Playwright scenarios run in Node, and their page.evaluate callbacks run in
  // the browser.
  {
    files: ["tests/**/*.mjs"],
    languageOptions: { globals: { ...globals.node, ...globals.browser } },
  },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [
      tseslint.configs.recommendedTypeChecked,
      reactHooks.configs.flat.recommended,
    ],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
  },
);
