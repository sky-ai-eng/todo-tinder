import { useState, useEffect } from 'react'
import type {
  CurrentUserIdentity,
  DeploymentConfig,
  TeamMember,
  TeamMembersResponse,
} from '../types'

/** In-flight Promise dedup for /api/config. The endpoint is read once at
 *  FE boot by AuthGate, but multiple components may still race to mount
 *  before the result lands; sharing one round-trip is courteous. */
let configInFlight: Promise<DeploymentConfig> | null = null

function loadConfig(): Promise<DeploymentConfig> {
  if (configInFlight) return configInFlight
  configInFlight = fetch('/api/config')
    .then((r) => {
      if (!r.ok) throw new Error(`/api/config: ${r.status}`)
      return r.json() as Promise<DeploymentConfig>
    })
    .finally(() => {
      configInFlight = null
    })
  return configInFlight
}

/** useDeploymentConfig fetches /api/config on every mount with in-flight
 *  dedup. The response carries only deployment_mode — AuthGate's
 *  pre-login signal for which login flow to render. */
export function useDeploymentConfig(): {
  config: DeploymentConfig | null
  loading: boolean
  error: string | null
} {
  const [config, setConfig] = useState<DeploymentConfig | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    loadConfig()
      .then((data) => {
        if (!cancelled) {
          setConfig(data)
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err))
          setLoading(false)
        }
      })
    return () => {
      cancelled = true
    }
  }, [])

  return { config, loading, error }
}

/** In-flight Promise dedup for /api/me. Multiple IdentityListField
 *  mounts in one render (e.g. a review-event predicate editing both
 *  author_in and reviewer_in) share one round-trip. No persistent
 *  cache — identity fields can change mid-session (user opens editor,
 *  configures Jira via Settings, returns), and caching would shadow
 *  real changes until reload. */
let meInFlight: Promise<CurrentUserIdentity | null> | null = null

function loadMe(): Promise<CurrentUserIdentity | null> {
  if (meInFlight) return meInFlight
  meInFlight = fetch('/api/me')
    .then((r) => {
      if (r.status === 401) return null
      if (!r.ok) throw new Error(`/api/me: ${r.status}`)
      return r.json() as Promise<CurrentUserIdentity>
    })
    .finally(() => {
      meInFlight = null
    })
  return meInFlight
}

/** useCurrentUserIdentity fetches /api/me on each mount with in-flight
 *  dedup, narrowed to the fields the predicate editor needs. Works in
 *  both modes (local mode synthesizes from the users row, multi mode
 *  reads via JWT-context query). Returns `me: null` on 401 so the editor
 *  can render a "not signed in" state rather than crashing on missing
 *  identity. */
export function useCurrentUserIdentity(): {
  me: CurrentUserIdentity | null
  loading: boolean
  error: string | null
} {
  const [me, setMe] = useState<CurrentUserIdentity | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    loadMe()
      .then((data) => {
        if (!cancelled) {
          setMe(data)
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err))
          setLoading(false)
        }
      })
    return () => {
      cancelled = true
    }
  }, [])

  return { me, loading, error }
}

/** useTeamMembers fetches the roster for the active user's team. Used
 *  by Variant B (multi-select) of the identity-allowlist field. Fetched
 *  fresh on each component mount — the roster is mutable during a
 *  session but cache invalidation isn't worth the websocket plumbing
 *  for v1. The list is usually small (single digits to low tens). */
export function useTeamMembers(): {
  members: TeamMember[]
  loading: boolean
  error: string | null
} {
  const [members, setMembers] = useState<TeamMember[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    fetch('/api/team/members')
      .then((r) => {
        if (!r.ok) throw new Error(`/api/team/members: ${r.status}`)
        return r.json() as Promise<TeamMembersResponse>
      })
      .then((data) => {
        if (!cancelled) {
          setMembers(data.members || [])
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err))
          setLoading(false)
        }
      })
    return () => {
      cancelled = true
    }
  }, [])

  return { members, loading, error }
}
