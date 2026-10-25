import react from '@vitejs/plugin-react'
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vite-plus'

export default defineConfig({
  plugins: [
    react(),
    {
      name: 'bundled-datastar',
      generateBundle() {
        this.emitFile({
          type: 'asset',
          fileName: '.keep',
          source: 'Build output placeholder.\n',
        })
      },
      configureServer(server) {
        server.middlewares.use((req, _res, next) => {
          if (req.url?.split('?')[0] === '/datastar.js') {
            req.url = '/node_modules/@starfederation/datastar/dist/bundles/datastar.js'
          }
          next()
        })
      },
    },
  ],
  publicDir: 'public',
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/admin': 'http://127.0.0.1:8080',
    },
  },
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    rolldownOptions: {
      input: {
        app: fileURLToPath(new URL('./index.html', import.meta.url)),
        datastar: fileURLToPath(
          new URL(
            './node_modules/@starfederation/datastar/dist/bundles/datastar.js',
            import.meta.url,
          ),
        ),
      },
      output: {
        entryFileNames: (chunk) =>
          chunk.name === 'datastar' ? 'datastar.js' : 'assets/[name]-[hash].js',
      },
    },
  },
  test: { include: ['src/**/*.test.ts'] },
  lint: { options: { typeAware: true, typeCheck: true } },
  fmt: {
    semi: false,
    singleQuote: true,
    ignorePatterns: ['src/contract.json'],
  },
})
