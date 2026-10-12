// Validates every ui/fixtures/*.json against its ui/schemas/*.json with ajv (draft 2020-12), so the
// schemas render.js relies on are actually checked.
import { readFile, readdir } from 'node:fs/promises'
import path from 'node:path'
// ajv's default export only understands draft-07/2019-09; the schemas here
// declare 2020-12, so the dedicated Ajv2020 build is required.
import Ajv2020 from 'ajv/dist/2020.js'

const ROOT = path.join(import.meta.dirname, '..')
const SCHEMAS_DIR = path.join(ROOT, 'schemas')
const FIXTURES_DIR = path.join(ROOT, 'fixtures')

async function main() {
  // strict:false then strictSchema back on: catches misspelled keywords/$refs without rejecting the
  // nullable unions (type: ["integer","null"]) these schemas use.
  const ajv = new Ajv2020({ strict: false, strictSchema: true })
  const schemaNames = (await readdir(SCHEMAS_DIR)).filter(f => f.endsWith('.json'))
  const fixtureNames = (await readdir(FIXTURES_DIR)).filter(f => f.endsWith('.json'))

  let checked = 0
  let failed = 0
  for (const name of fixtureNames) {
    if (!schemaNames.includes(name)) {
      console.error(`FAIL ${name}: no matching schema in sleeper/ui/schemas/`)
      failed++
      continue
    }
    checked++
    const schema = JSON.parse(await readFile(path.join(SCHEMAS_DIR, name), 'utf8'))
    const data = JSON.parse(await readFile(path.join(FIXTURES_DIR, name), 'utf8'))
    const validate = ajv.compile(schema)
    if (!validate(data)) {
      console.error(`FAIL ${name}: ${ajv.errorsText(validate.errors, { separator: '; ' })}`)
      failed++
    }
  }
  const unusedSchemas = schemaNames.filter(n => !fixtureNames.includes(n))
  if (unusedSchemas.length) {
    console.error(`FAIL: schema(s) with no fixture to validate: ${unusedSchemas.join(', ')}`)
    failed++
  }

  console.log(`validate-schemas: ${checked} fixtures checked against their schema, ${failed} failure(s)`)
  if (failed > 0) process.exit(1)
}

main().catch(err => { console.error(err); process.exit(1) })
