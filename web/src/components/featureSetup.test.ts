import { describe, it, expect } from 'vitest'
import { featureSetup } from './featureSetup'
import config from '../../../configs/vaults3.yaml?raw'

// The setup shown in Settings must be config the server actually reads: a
// misspelled section is silently ignored, so the feature would stay off.
describe('feature setup snippets', () => {
  const sections = new Set([...config.matchAll(/^([a-z_]+):/gm)].map(m => m[1]))

  it('names only top level sections that configs/vaults3.yaml defines', () => {
    for (const [feature, snippet] of Object.entries(featureSetup)) {
      for (const m of snippet.matchAll(/^([a-z_]+):/gm)) {
        expect({ feature, section: m[1], known: sections.has(m[1]) }).toEqual({ feature, section: m[1], known: true })
      }
    }
  })

  it('turns the feature on', () => {
    for (const [feature, snippet] of Object.entries(featureSetup)) {
      expect({ feature, on: /enabled: true|^debug: true/m.test(snippet) }).toEqual({ feature, on: true })
    }
  })
})
