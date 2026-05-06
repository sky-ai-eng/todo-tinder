import { useEffect, useRef, useCallback } from 'react'
import type { WSEvent } from '../types'
import { toastStore } from '../components/Toast/toastStore'

type Handler = (event: WSEvent) => void

// --- Singleton connection manager ---
// Lives outside React's lifecycle so StrictMode double-mounts and page
// navigations don't tear down the socket.

let globalWs: WebSocket | null = null
const listeners = new Set<Handler>()

// Track per-repo clone_status across WS events so we only fire the
// "clone failed" toast on the *transition* into 'failed', not on every
// repo_profile_updated event with the same failed status. Module-level
// (not React state) so the dedupe survives page navigations and the
// short-lived useWebSocket subscriptions on individual pages.
const cloneStatusByRepo = new Map<string, 'ok' | 'failed' | 'pending'>()

function ensureConnected() {
  if (
    globalWs &&
    (globalWs.readyState === WebSocket.OPEN || globalWs.readyState === WebSocket.CONNECTING)
  ) {
    return
  }

  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const ws = new WebSocket(`${proto}//${window.location.host}/api/ws`)

  ws.onmessage = (e) => {
    try {
      const event = JSON.parse(e.data) as WSEvent
      // Global handler: any toast event goes straight into the store, no
      // per-page listener required. Keeps consumers ignorant of WS plumbing.
      if (event.type === 'toast') {
        toastStore.push({
          id: event.data.id,
          level: event.data.level,
          title: event.data.title,
          body: event.data.body,
        })
        return
      }
      // Cross-page clone failure surfacing: when a repo's clone_status
      // transitions to 'failed' on the backend (bootstrap, lazy clone,
      // or import path), fire a sticky error toast with a CTA to the
      // Repos page. Doing it here (rather than in Repos.tsx) means the
      // user sees it even when they're on Board / Settings / Tasks.
      if (event.type === 'repo_profile_updated' && event.data && typeof event.data === 'object') {
        const data = event.data as {
          id?: string
          clone_status?: 'ok' | 'failed' | 'pending'
          clone_error_kind?: 'ssh' | 'other'
        }
        if (data.id && data.clone_status) {
          const prev = cloneStatusByRepo.get(data.id)
          cloneStatusByRepo.set(data.id, data.clone_status)
          if (data.clone_status === 'failed' && prev !== 'failed') {
            const kind = data.clone_error_kind === 'ssh' ? ' (SSH)' : ''
            toastStore.push({
              level: 'error',
              title: 'Clone failed',
              body: `Could not clone ${data.id}${kind}. Open the Repos page for details.`,
              action: { label: 'Go to Repos', to: '/repos' },
            })
          }
        }
      }
      for (const fn of listeners) {
        fn(event)
      }
    } catch {
      // ignore non-JSON messages (pings, etc.)
    }
  }

  ws.onclose = () => {
    globalWs = null
    // Only reconnect if there are still listeners
    if (listeners.size > 0) {
      setTimeout(ensureConnected, 2000)
    }
  }

  globalWs = ws
}

function subscribe(handler: Handler) {
  listeners.add(handler)
  ensureConnected()

  return () => {
    listeners.delete(handler)
    // Don't close — other pages may still need the connection.
    // The socket will naturally stop reconnecting when listeners hits 0.
  }
}

// --- React hook ---

export function useWebSocket(handler: Handler) {
  // Latest-ref pattern: keep a mutable reference to the freshest handler
  // closure so the stable wrapper below always dispatches to it, without
  // having to re-subscribe on every render. The assignment lives in an
  // effect (not inline during render) per react-hooks/refs.
  const handlerRef = useRef(handler)
  useEffect(() => {
    handlerRef.current = handler
  }, [handler])

  // Stable wrapper so the subscription identity doesn't change on re-renders
  const stableHandler = useCallback((event: WSEvent) => {
    handlerRef.current(event)
  }, [])

  useEffect(() => {
    return subscribe(stableHandler)
  }, [stableHandler])
}
