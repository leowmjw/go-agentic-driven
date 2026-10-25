import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { isDeepStrictEqual } from 'node:util'

const emitted = execFileSync('go', ['run', './cmd/server', '-contract'], {
  cwd: new URL('../../', import.meta.url),
  encoding: 'utf8',
})
const tracked = readFileSync(new URL('../src/contract.json', import.meta.url), 'utf8')
if (!isDeepStrictEqual(JSON.parse(emitted), JSON.parse(tracked))) {
  console.error(
    'Contract drift: regenerate frontend/src/contract.json with go run ./cmd/server -contract',
  )
  process.exitCode = 1
} else {
  console.log('Frontend contract matches backend emission.')
}
