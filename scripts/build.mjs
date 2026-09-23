#!/usr/bin/env node
/**
 * 一键构建：前端生产包 + 后端二进制。
 *
 * 用法：
 *   node scripts/build.mjs              # 构建到 dist/
 *   node scripts/build.mjs --skip-frontend
 *   node scripts/build.mjs --skip-backend
 *
 * 产物：
 *   dist/codeagent-server[.exe]   后端可执行文件（内嵌版本信息）
 *   dist/web/                     前端静态资源（交给 Nginx 或后端静态托管）
 */
import { execFileSync } from 'node:child_process'
import { cpSync, existsSync, mkdirSync, rmSync, statSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const root = resolve(__dirname, '..')
const dist = join(root, 'dist')
const isWindows = process.platform === 'win32'
const skipFrontend = process.argv.includes('--skip-frontend')
const skipBackend = process.argv.includes('--skip-backend')

mkdirSync(dist, { recursive: true })

function run(cmd, args, cwd) {
  console.log(`\n$ ${cmd} ${args.join(' ')}   (cwd=${cwd})`)
  execFileSync(cmd, args, { cwd, stdio: 'inherit', shell: isWindows })
}

function human(bytes) {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}

if (!skipBackend) {
  const bin = join(dist, isWindows ? 'codeagent-server.exe' : 'codeagent-server')
  run('go', ['build', '-trimpath', '-ldflags', '-s -w', '-o', bin, './cmd/server'], join(root, 'backend'))
  console.log(`后端产物: ${bin}  (${human(statSync(bin).size)})`)
}

if (!skipFrontend) {
  const fe = join(root, 'frontend')
  if (!existsSync(join(fe, 'node_modules'))) {
    console.log('未发现 node_modules，先执行 pnpm install …')
    run('pnpm', ['install'], fe)
  }
  run('pnpm', ['run', 'build'], fe)
  const webDist = join(dist, 'web')
  rmSync(webDist, { recursive: true, force: true })
  cpSync(join(fe, 'dist'), webDist, { recursive: true })
  console.log(`前端产物: ${webDist}`)
}

console.log('\n构建完成。运行方式：')
console.log(`  1) 后端: ./dist/codeagent-server${isWindows ? '.exe' : ''} --config deploy/config.example.json`)
console.log('  2) 前端: 将 dist/web 交给 Nginx 托管，开发态用 cd frontend && pnpm run dev')
