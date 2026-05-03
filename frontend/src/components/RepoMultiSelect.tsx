import { useState, useEffect, useMemo, useCallback } from 'react'
import { Check, X } from 'lucide-react'
import { readError } from '../lib/api'
import { toast } from './Toast/toastStore'

interface RepoProfile {
  id: string
  owner: string
  repo: string
}

interface Props {
  value: string[]
  onChange: (next: string[]) => void
}

// RepoMultiSelect is the project page's pinned-repos picker. It reads
// from /api/repos (configured-repos list) and exposes those slugs as
// toggleable chips. Mirroring the server-side validation contract:
// the user can only pick from the configured set, so the chip strip
// already enforces what validatePinnedRepos enforces server-side.
//
// Chosen slugs render up top; the popover below holds the remaining
// configured options + a search filter. Empty configured list shows
// a hint pointing at /repos rather than an awkward empty popover.
export default function RepoMultiSelect({ value, onChange }: Props) {
  const [available, setAvailable] = useState<RepoProfile[]>([])
  const [loading, setLoading] = useState(true)
  const [search, setSearch] = useState('')

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const res = await fetch('/api/repos')
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to load repos'))
          return
        }
        const data: RepoProfile[] = await res.json()
        if (!cancelled) setAvailable(data)
      } catch (err) {
        if (!cancelled) {
          toast.error(`Failed to load repos: ${err instanceof Error ? err.message : String(err)}`)
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    load()
    return () => {
      cancelled = true
    }
  }, [])

  const selected = useMemo(() => new Set(value), [value])
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    if (!q) return available
    return available.filter((r) => r.id.toLowerCase().includes(q))
  }, [available, search])

  const toggle = useCallback(
    (slug: string) => {
      const next = new Set(value)
      if (next.has(slug)) {
        next.delete(slug)
      } else {
        next.add(slug)
      }
      onChange(Array.from(next).sort())
    },
    [value, onChange],
  )

  if (loading) {
    return <div className="text-[12px] text-text-tertiary py-2">Loading repos…</div>
  }

  if (available.length === 0) {
    return (
      <div className="text-[12px] text-text-tertiary py-2">
        No repos configured.{' '}
        <a href="/repos" className="text-accent hover:underline">
          Add repos
        </a>{' '}
        first.
      </div>
    )
  }

  return (
    <div>
      {value.length > 0 && (
        <div className="flex flex-wrap gap-1.5 mb-2">
          {value.map((slug) => (
            <button
              key={slug}
              type="button"
              onClick={() => toggle(slug)}
              className="
                inline-flex items-center gap-1 rounded-full
                bg-accent-soft text-accent px-2.5 py-0.5 text-[11px]
                hover:bg-accent hover:text-white transition-colors
                group
              "
            >
              {slug}
              <X size={10} className="opacity-60 group-hover:opacity-100" />
            </button>
          ))}
        </div>
      )}
      <input
        type="text"
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        placeholder="Search configured repos…"
        className="
          w-full rounded-lg border border-border-subtle
          bg-white/60 px-3 py-1.5 text-[12px] text-text-primary
          placeholder:text-text-tertiary mb-1.5
          focus:outline-none focus:border-accent focus:bg-white
        "
      />
      <div className="max-h-40 overflow-y-auto rounded-lg border border-border-subtle bg-white/40">
        {filtered.length === 0 ? (
          <div className="text-[12px] text-text-tertiary py-2 px-3">No matches.</div>
        ) : (
          filtered.map((repo) => {
            const isSelected = selected.has(repo.id)
            return (
              <button
                key={repo.id}
                type="button"
                onClick={() => toggle(repo.id)}
                className="
                  w-full flex items-center justify-between gap-2
                  px-3 py-1.5 text-[12px] text-left
                  hover:bg-black/[0.03] transition-colors
                "
              >
                <span className="text-text-primary truncate">{repo.id}</span>
                {isSelected && <Check size={12} className="shrink-0 text-accent" />}
              </button>
            )
          })
        )}
      </div>
    </div>
  )
}
