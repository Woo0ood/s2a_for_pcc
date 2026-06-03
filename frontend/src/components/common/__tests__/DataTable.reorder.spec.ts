import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { ref } from 'vue'
import DataTable from '../DataTable.vue'
import type { Column } from '../types'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key, locale: ref('en') }),
}))

vi.mock('vue-router', () => ({
  useRoute: () => ({ name: undefined, path: undefined }),
}))

// DataTable calls window.matchMedia on mount; jsdom lacks it.
beforeEach(() => {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: true, // force desktop <table> render (not mobile cards)
    media: query,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: vi.fn(),
  })) as unknown as typeof window.matchMedia
})

afterEach(() => {
  localStorage.clear()
  vi.restoreAllMocks()
})

const columns: Column[] = [
  { key: 'name', label: 'Name' },
  { key: 'status', label: 'Status' },
  { key: 'usage', label: 'Usage' },
  { key: 'actions', label: 'Actions' },
]
const data = [{ id: 1, name: 'a', status: 'ok', usage: 1 }]

const headerKeys = (wrapper: ReturnType<typeof mount>) =>
  wrapper.findAll('thead th').map((th) => th.attributes('data-col-key'))

const mountTable = (props: Record<string, unknown> = {}) =>
  mount(DataTable, {
    props: { columns, data, tableId: 'TestTable', ...props },
    global: { stubs: { Icon: true } },
  })

describe('DataTable column order', () => {
  it('renders columns in natural order when nothing is persisted', () => {
    const wrapper = mountTable()
    expect(headerKeys(wrapper)).toEqual(['name', 'status', 'usage', 'actions'])
  })

  it('applies a persisted order, keeping actions pinned last', () => {
    localStorage.setItem('s2a:colorder:TestTable', JSON.stringify(['usage', 'name', 'status']))
    const wrapper = mountTable()
    expect(headerKeys(wrapper)).toEqual(['usage', 'name', 'status', 'actions'])
  })

  it('ignores persisted order when reorderable is false', () => {
    localStorage.setItem('s2a:colorder:TestTable', JSON.stringify(['usage', 'name', 'status']))
    const wrapper = mountTable({ reorderable: false })
    expect(headerKeys(wrapper)).toEqual(['name', 'status', 'usage', 'actions'])
  })
})
