import { useState, useEffect, useCallback } from 'react'
import type { Prompt } from '../types'
import PromptDrawer from '../components/PromptDrawer'

export default function Prompts() {
  const [prompts, setPrompts] = useState<Prompt[]>([])
  const [loading, setLoading] = useState(true)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [isNew, setIsNew] = useState(false)

  const fetchPrompts = useCallback(async () => {
    try {
      const res = await fetch('/api/prompts')
      if (res.ok) {
        setPrompts(await res.json())
      }
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchPrompts() }, [fetchPrompts])

  const openNew = () => {
    setSelectedId(null)
    setIsNew(true)
  }

  const openEdit = (id: string) => {
    setIsNew(false)
    setSelectedId(id)
  }

  const closeDrawer = () => {
    setSelectedId(null)
    setIsNew(false)
  }

  const handleSaved = () => {
    closeDrawer()
    fetchPrompts()
  }

  const handleDeleted = () => {
    closeDrawer()
    fetchPrompts()
  }

  return (
    <div className="max-w-3xl mx-auto">
      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-[17px] font-semibold text-text-primary">Prompts</h1>
          <p className="text-[13px] text-text-tertiary mt-0.5">
            Delegation strategies for your triage events
          </p>
        </div>
        <button
          onClick={openNew}
          className="text-[13px] font-semibold text-white bg-accent hover:bg-accent/90 px-4 py-2 rounded-full transition-colors"
        >
          New Prompt
        </button>
      </div>

      {/* Prompt list */}
      {loading ? (
        <div className="space-y-3">
          {[...Array(3)].map((_, i) => (
            <div key={i} className="h-[100px] rounded-2xl bg-black/[0.03] animate-pulse" />
          ))}
        </div>
      ) : prompts.length === 0 ? (
        <div className="text-center py-16">
          <p className="text-text-tertiary text-sm mb-4">No prompts yet</p>
          <button
            onClick={openNew}
            className="text-[13px] font-medium text-accent hover:text-accent/80 transition-colors"
          >
            Create your first prompt
          </button>
        </div>
      ) : (
        <div className="space-y-3">
          {prompts.map(prompt => (
            <button
              key={prompt.id}
              onClick={() => openEdit(prompt.id)}
              className="w-full text-left p-5 rounded-2xl border border-border-subtle bg-surface-raised/60 hover:bg-surface-raised hover:border-accent/20 hover:shadow-sm transition-all duration-150 group"
            >
              <div className="flex items-center gap-3 mb-2">
                <h3 className="text-[14px] font-semibold text-text-primary group-hover:text-accent transition-colors">
                  {prompt.name}
                </h3>
                {prompt.source === 'system' && (
                  <span className="text-[9px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded bg-black/[0.04] text-text-tertiary">
                    System
                  </span>
                )}
                {prompt.usage_count > 0 && (
                  <span className="text-[10px] text-text-tertiary ml-auto">
                    Used {prompt.usage_count}x
                  </span>
                )}
              </div>
              <p className="text-[12px] text-text-tertiary line-clamp-2 leading-relaxed font-mono">
                {prompt.body.slice(0, 200)}{prompt.body.length > 200 ? '...' : ''}
              </p>
            </button>
          ))}
        </div>
      )}

      <PromptDrawer
        promptId={selectedId}
        isNew={isNew}
        onClose={closeDrawer}
        onSaved={handleSaved}
        onDeleted={handleDeleted}
      />
    </div>
  )
}
