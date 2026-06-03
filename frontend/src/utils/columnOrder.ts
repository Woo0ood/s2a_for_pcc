import type { Column } from '@/components/common/types'

/**
 * Apply a saved column order to a set of columns.
 * - A leading `select` column (only when it is index 0) stays pinned first.
 * - An `actions` column stays pinned last.
 * - Remaining ("movable") columns are ordered by `order` (array of column keys);
 *   keys in `order` that no longer exist are ignored, and columns not present in
 *   `order` are appended at the end of the movable group in their original order.
 */
export function applyColumnOrder(columns: Column[], order: string[]): Column[] {
  const leading: Column[] = []
  const trailing: Column[] = []
  const middle: Column[] = []

  columns.forEach((col, index) => {
    if (index === 0 && col.key === 'select') {
      leading.push(col)
    } else if (col.key === 'actions') {
      trailing.push(col)
    } else {
      middle.push(col)
    }
  })

  const byKey = new Map(middle.map((col) => [col.key, col]))
  const ordered: Column[] = []
  for (const key of order) {
    const col = byKey.get(key)
    if (col) {
      ordered.push(col)
      byKey.delete(key)
    }
  }
  for (const col of middle) {
    if (byKey.has(col.key)) ordered.push(col)
  }

  return [...leading, ...ordered, ...trailing]
}
