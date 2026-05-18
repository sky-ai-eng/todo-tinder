import type { Task } from '../../types'

// ColumnFilterState is the shape every BoardColumn's filter strip
// reads + writes. Lives in its own file (not BoardColumn.tsx) so
// React Fast Refresh stays happy — the rule is "a component file
// exports only components."
export type ColumnFilterState = {
  search: string
  source: 'all' | 'github' | 'jira'
}

export const emptyFilter: ColumnFilterState = { search: '', source: 'all' }

// applyColumnFilter is the canonical filter pass — used by every
// column so behavior stays uniform. Empty filter = pass-through.
// Matches the pre-SKY-330 queue sidebar's filter logic (title,
// source_id, ai_summary, event_type) so users don't lose any of the
// search affordances they were relying on.
export function applyColumnFilter(tasks: Task[], f: ColumnFilterState): Task[] {
  let items = tasks
  if (f.source !== 'all') {
    items = items.filter((t) => t.source === f.source)
  }
  const q = f.search.trim().toLowerCase()
  if (q) {
    items = items.filter(
      (t) =>
        t.title.toLowerCase().includes(q) ||
        t.source_id.toLowerCase().includes(q) ||
        (t.ai_summary ?? '').toLowerCase().includes(q) ||
        t.event_type.toLowerCase().includes(q),
    )
  }
  return items
}
