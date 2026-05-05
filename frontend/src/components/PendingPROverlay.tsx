import { useState, useEffect, useCallback } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { parseDiff } from 'react-diff-view'
import type { FileData } from 'react-diff-view'
import DiffFile from './DiffFile'
import PendingPRSummary from './PendingPRSummary'

// PendingPR mirrors the JSON shape internal/server/pending_prs_handler.go
// returns. submitted_at is omitted on the wire when nil; the field
// stays optional here so the frontend can tell the row apart from a
// row that was just created (locked=false, no submitted_at) from one
// mid-submit (locked=true, submitted_at present but row not yet
// deleted because GitHub failed).
interface PendingPR {
  id: string
  run_id: string
  owner: string
  repo: string
  head_branch: string
  head_sha: string
  base_branch: string
  title: string
  body: string
  locked: boolean
  submitted_at?: string
}

interface Props {
  runID: string
  open: boolean
  onClose: () => void
}

// PendingPROverlay is the modal the user opens from a delegated
// run's "Open PR" button. Mirrors ReviewOverlay's shape (backdrop +
// centered panel + summary + diff list) but with PR-specific
// affordances:
//   - title editor (reviews don't have a title)
//   - draft checkbox at submit time
//   - no inline-comment surface (commentsByFile is always empty)
//
// The diff comes from the bare clone via /api/pending-prs/{id}/diff
// rather than from GitHub's diff API — the PR doesn't exist yet, so
// there's nothing to fetch from there. Server-side livePRDiff fetches
// origin/{head} into the bare's local ref before computing the diff
// (the agent's `git push` doesn't auto-sync the bare).
export default function PendingPROverlay({ runID, open, onClose }: Props) {
  const [pr, setPR] = useState<PendingPR | null>(null)
  const [files, setFiles] = useState<FileData[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [draft, setDraft] = useState(false)
  const prId = pr?.id

  // Fetch PR + diff
  useEffect(() => {
    if (!open || !runID) return
    let cancelled = false
    setLoading(true)
    setError(null)
    ;(async () => {
      try {
        const prRes = await fetch(`/api/agent/runs/${runID}/pending-pr`)
        if (!prRes.ok) throw new Error('Failed to load pending PR')
        const prData: PendingPR = await prRes.json()
        if (cancelled) return
        setPR(prData)

        const diffRes = await fetch(`/api/pending-prs/${prData.id}/diff`)
        if (!diffRes.ok) throw new Error('Failed to load diff')
        const diffText = await diffRes.text()
        if (cancelled) return
        const parsed = parseDiff(diffText)
        setFiles(parsed)
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()

    return () => {
      cancelled = true
    }
  }, [open, runID])

  // Title + body updates — explicit-Save model (PendingPRSummary's
  // Edit / Save / Cancel buttons drive this), not autosave.
  const handleUpdateTitle = useCallback(
    async (title: string) => {
      if (!prId) return
      setPR((prev) => (prev ? { ...prev, title } : prev))
      await fetch(`/api/pending-prs/${prId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title }),
      })
    },
    [prId],
  )

  const handleUpdateBody = useCallback(
    async (body: string) => {
      if (!prId) return
      setPR((prev) => (prev ? { ...prev, body } : prev))
      await fetch(`/api/pending-prs/${prId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ body }),
      })
    },
    [prId],
  )

  // Draft is local state only until submit — it's not persisted on
  // the row (the schema deliberately doesn't have a draft column;
  // the value rides through to GitHub at submit time).
  const handleUpdateDraft = useCallback((next: boolean) => {
    setDraft(next)
  }, [])

  const handleSubmit = useCallback(async () => {
    if (!prId) return
    setSubmitting(true)
    try {
      const res = await fetch(`/api/pending-prs/${prId}/submit`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ draft }),
      })
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        throw new Error(data.error || 'Submit failed')
      }
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSubmitting(false)
    }
  }, [prId, draft, onClose])

  // Close on Escape
  useEffect(() => {
    if (!open) return
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [open, onClose])

  return (
    <AnimatePresence>
      {open && (
        <>
          {/* Backdrop */}
          <motion.div
            className="fixed inset-0 z-50 bg-black/20 backdrop-blur-sm"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Panel */}
          <motion.div
            className="fixed inset-6 z-50 flex flex-col bg-surface/95 backdrop-blur-2xl border border-border-glass rounded-3xl shadow-2xl shadow-black/[0.08] overflow-hidden"
            initial={{ opacity: 0, scale: 0.97, y: 12 }}
            animate={{ opacity: 1, scale: 1, y: 0 }}
            exit={{ opacity: 0, scale: 0.97, y: 12 }}
            transition={{ type: 'spring', damping: 30, stiffness: 350 }}
            onClick={(e) => e.stopPropagation()}
          >
            {/* Top bar */}
            <div className="shrink-0 flex items-center justify-between px-6 py-4 border-b border-border-subtle">
              <div className="flex items-center gap-3">
                <div className="w-2 h-2 rounded-full bg-snooze animate-pulse" />
                <h1 className="text-[15px] font-semibold text-text-primary tracking-tight">
                  Pending PR
                </h1>
                {pr && (
                  <span className="text-[12px] text-text-tertiary font-mono">
                    {pr.owner}/{pr.repo}
                  </span>
                )}
              </div>
              <button
                onClick={onClose}
                className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-2 py-1 rounded-lg hover:bg-black/[0.03]"
              >
                &times;
              </button>
            </div>

            {/* Content */}
            <div className="flex-1 overflow-y-auto">
              {loading ? (
                <div className="flex items-center justify-center h-64">
                  <div className="flex flex-col items-center gap-3">
                    <div className="w-5 h-5 border-2 border-accent/30 border-t-accent rounded-full animate-spin" />
                    <span className="text-[12px] text-text-tertiary">Loading PR...</span>
                  </div>
                </div>
              ) : error ? (
                <div className="flex items-center justify-center h-64">
                  <div className="text-center">
                    <p className="text-[13px] text-dismiss">{error}</p>
                    <button
                      onClick={onClose}
                      className="text-[12px] text-text-tertiary hover:text-text-secondary mt-2 transition-colors"
                    >
                      Close
                    </button>
                  </div>
                </div>
              ) : pr ? (
                <div className="p-6 space-y-4 max-w-5xl mx-auto">
                  <PendingPRSummary
                    owner={pr.owner}
                    repo={pr.repo}
                    headBranch={pr.head_branch}
                    baseBranch={pr.base_branch}
                    headSHA={pr.head_sha}
                    title={pr.title}
                    body={pr.body}
                    draft={draft}
                    onUpdateTitle={handleUpdateTitle}
                    onUpdateBody={handleUpdateBody}
                    onUpdateDraft={handleUpdateDraft}
                    onSubmit={handleSubmit}
                    onClose={onClose}
                    submitting={submitting}
                  />

                  {/* Diff files — same DiffFile component the review
                      overlay uses; commentsByFile is always empty for
                      pending PRs (no inline-comment surface at create
                      time). */}
                  <div className="space-y-3">
                    {files.map((file, i) => {
                      const path = file.newPath === '/dev/null' ? file.oldPath : file.newPath
                      return (
                        <DiffFile
                          key={path + i}
                          file={file}
                          comments={[]}
                          defaultCollapsed={files.length > 8}
                          onUpdateComment={() => {}}
                          onDeleteComment={() => {}}
                        />
                      )
                    })}
                  </div>

                  {files.length === 0 && (
                    <div className="text-center py-12">
                      <p className="text-[13px] text-text-tertiary">No diff available</p>
                    </div>
                  )}
                </div>
              ) : null}
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}
