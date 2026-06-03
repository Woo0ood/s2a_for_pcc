import { afterEach, describe, expect, it } from 'vitest'
import {
  applyColumnOrder,
  buildColumnOrderStorageKey,
  readColumnOrder,
  writeColumnOrder,
} from '@/utils/columnOrder'
import type { Column } from '@/components/common/types'

const cols = (...keys: string[]): Column[] => keys.map((key) => ({ key, label: key }))

describe('applyColumnOrder', () => {
  it('returns columns unchanged when order is empty', () => {
    const input = cols('name', 'status', 'usage')
    expect(applyColumnOrder(input, []).map((c) => c.key)).toEqual(['name', 'status', 'usage'])
  })

  it('reorders middle columns by the saved order', () => {
    const input = cols('name', 'status', 'usage')
    expect(applyColumnOrder(input, ['usage', 'name', 'status']).map((c) => c.key)).toEqual([
      'usage', 'name', 'status',
    ])
  })

  it('keeps leading select column pinned first', () => {
    const input = cols('select', 'name', 'status')
    expect(applyColumnOrder(input, ['status', 'name', 'select']).map((c) => c.key)).toEqual([
      'select', 'status', 'name',
    ])
  })

  it('keeps actions column pinned last', () => {
    const input = cols('name', 'status', 'actions')
    expect(applyColumnOrder(input, ['actions', 'status', 'name']).map((c) => c.key)).toEqual([
      'status', 'name', 'actions',
    ])
  })

  it('pins both select (first) and actions (last)', () => {
    const input = cols('select', 'name', 'status', 'actions')
    expect(applyColumnOrder(input, ['status', 'name']).map((c) => c.key)).toEqual([
      'select', 'status', 'name', 'actions',
    ])
  })

  it('ignores saved keys that no longer exist', () => {
    const input = cols('name', 'status')
    expect(applyColumnOrder(input, ['gone', 'status', 'name']).map((c) => c.key)).toEqual([
      'status', 'name',
    ])
  })

  it('appends new (unsaved) columns at the end of the middle group in original order', () => {
    const input = cols('name', 'status', 'newA', 'newB')
    expect(applyColumnOrder(input, ['status', 'name']).map((c) => c.key)).toEqual([
      'status', 'name', 'newA', 'newB',
    ])
  })

  it('does not treat a non-first select as pinned', () => {
    const input = cols('name', 'select')
    expect(applyColumnOrder(input, ['select', 'name']).map((c) => c.key)).toEqual([
      'select', 'name',
    ])
  })
})

describe('column order storage', () => {
  afterEach(() => localStorage.clear())

  it('builds a namespaced storage key', () => {
    expect(buildColumnOrderStorageKey('AdminUsers')).toBe('s2a:colorder:AdminUsers')
  })

  it('writes and reads back an order array', () => {
    const key = buildColumnOrderStorageKey('t1')
    writeColumnOrder(key, ['a', 'b', 'c'])
    expect(readColumnOrder(key)).toEqual(['a', 'b', 'c'])
  })

  it('returns [] when nothing is stored', () => {
    expect(readColumnOrder('s2a:colorder:missing')).toEqual([])
  })

  it('returns [] and does not throw on corrupted json', () => {
    localStorage.setItem('s2a:colorder:bad', '{not json')
    expect(readColumnOrder('s2a:colorder:bad')).toEqual([])
  })

  it('filters non-string entries out of stored arrays', () => {
    localStorage.setItem('s2a:colorder:mixed', JSON.stringify(['a', 1, null, 'b']))
    expect(readColumnOrder('s2a:colorder:mixed')).toEqual(['a', 'b'])
  })
})
