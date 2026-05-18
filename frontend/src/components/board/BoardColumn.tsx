import { useMemo } from 'react'
import { useDroppable } from '@dnd-kit/core'
import EventBadge from '../EventBadge'
import type { Task } from '../../types'
import type { ColumnFilterState } from './columnFilter'

// BoardColumn is SKY-330's reusable column shell. Wraps a droppable
// area with a header (title + count) and an optional per-column
// filter (search + source toggle + event-type chips, generalised from
// the queue sidebar's filter UI). Same column widths the pre-redesign
// board used so the horizontal scroll snaps the way SKY-330 specifies.
//
// Filter state is owned by the parent so each column has its own
// independent filter context. localStorage persistence happens at the
// parent level (Board.tsx) keyed by columnId. Filter helpers
// (emptyFilter, applyColumnFilter) live in ./columnFilter.ts so this
// file exports only components (React Fast Refresh constraint).

interface Props {
  id: string
  title: string
  totalCount: number
  filteredCount: number
  tasks: Task[] // raw list, used to populate the event-type chips
  filter: ColumnFilterState
  onFilterChange: (next: ColumnFilterState) => void
  // Optional header slot for column-specific affordances (e.g. the
  // snoozed toggle on Queued, "see more" on Done). Renders to the
  // right of the title/count.
  headerExtra?: React.ReactNode
  // SKY-330: Queued is the leftmost column under the horizontal-scroll
  // layout; its width should match the rest. width="standard" gives the
  // pre-redesign 3-column-grid width; downstream we can add "wide" if
  // we want one column to breathe.
  children: React.ReactNode
}

export default function BoardColumn({
  id,
  title,
  totalCount,
  filteredCount,
  tasks,
  filter,
  onFilterChange,
  headerExtra,
  children,
}: Props) {
  const { setNodeRef, isOver } = useDroppable({ id, data: { type: 'column' } })

  // Event-type chips reflect the unfiltered set so the user always
  // sees what types exist in this column, even after filtering them
  // out. Matches the old queue sidebar's chip behavior.
  const eventTypes = useMemo(() => {
    const seen = new Set<string>()
    for (const t of tasks) {
      if (t.event_type) seen.add(t.event_type)
    }
    return Array.from(seen)
  }, [tasks])

  const showFilteredCount = filteredCount !== totalCount

  // SKY-330: column width matches the pre-redesign grid-cols-3
  // column. Fixed width here rather than fluid because the board
  // is horizontally scrollable — a viewport-relative width would
  // compress cards as the user scrolls to reveal additional columns.
  // 520px gives AgentCard log content room to breathe (it was the
  // workhorse card pre-redesign at ~480-520px on typical viewports).
  return (
    <div className="flex flex-col w-[520px] shrink-0">
      <div className="flex items-center justify-between mb-3 px-1">
        <div className="flex items-center gap-2">
          <h2 className="text-[13px] font-medium text-text-secondary">{title}</h2>
          <span className="text-[11px] text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5">
            {showFilteredCount ? `${filteredCount}/${totalCount}` : totalCount}
          </span>
        </div>
        {headerExtra}
      </div>

      {/* Filter strip — collapses into a one-line search by default. */}
      <ColumnFilter filter={filter} onChange={onFilterChange} eventTypes={eventTypes} />

      <div
        ref={setNodeRef}
        className={`flex-1 rounded-2xl border bg-black/[0.01] p-3 space-y-3 overflow-y-auto max-h-[calc(100vh-220px)] transition-colors ${
          isOver ? 'border-accent/30 bg-accent/[0.03]' : 'border-border-subtle'
        }`}
      >
        {children}
      </div>
    </div>
  )
}

function ColumnFilter({
  filter,
  onChange,
  eventTypes,
}: {
  filter: ColumnFilterState
  onChange: (next: ColumnFilterState) => void
  eventTypes: string[]
}) {
  return (
    <div className="mb-2 px-1 space-y-1">
      <input
        type="text"
        placeholder="Search..."
        value={filter.search}
        onChange={(e) => onChange({ ...filter, search: e.target.value })}
        className="w-full bg-white/50 border border-border-subtle rounded-lg px-2 py-1 text-[11px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-1 focus:ring-accent/30 focus:border-accent/40 transition-colors"
      />
      <div className="flex items-center gap-1 flex-wrap">
        {(['all', 'github', 'jira'] as const).map((f) => (
          <button
            key={f}
            type="button"
            onClick={() => onChange({ ...filter, source: f })}
            className={`text-[10px] px-1.5 py-0.5 rounded-full transition-colors ${
              filter.source === f
                ? 'bg-accent/10 text-accent font-medium'
                : 'text-text-tertiary hover:text-text-secondary'
            }`}
          >
            {f === 'all' ? 'All' : f === 'github' ? 'GH' : 'Jira'}
          </button>
        ))}
        {eventTypes.length > 0 && eventTypes.length <= 6 && (
          <span className="text-text-tertiary/40 text-[10px] mx-1">·</span>
        )}
        {eventTypes.length > 0 &&
          eventTypes.length <= 6 &&
          eventTypes.map((et) => (
            <button
              key={et}
              type="button"
              onClick={() => onChange({ ...filter, search: et })}
              className="opacity-70 hover:opacity-100 transition-opacity"
              title={`Filter by ${et}`}
            >
              <EventBadge eventType={et} compact />
            </button>
          ))}
      </div>
    </div>
  )
}
