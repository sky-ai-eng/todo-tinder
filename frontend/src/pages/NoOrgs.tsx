import { useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '../contexts/AuthContext'

/**
 * NoOrgs is the rare-state landing page for users with zero org
 * memberships (SKY-345). After auto-provisioning shipped, the 95%
 * case is that signup creates a personal org and the user is routed
 * straight into the app — so landing here in steady state means one
 * of three things:
 *
 *   1. Invite-pending — the instance has a pending invitation for this
 *      account (org_invitations table). TODO when the invite flow
 *      lands; today we don't have the data, so this branch is a no-op.
 *   2. Admin-gated — the instance is `invite-only` and no invite
 *      exists for this user. Surface "ask your admin" copy.
 *   3. Catch-all — shouldn't happen post-provisioning, but defensive
 *      so a never-expected state has SOMETHING actionable.
 *
 * The branch choice keys off `auth.me.join_policy`. Local mode never
 * mounts AuthContext, so this component is multi-mode only.
 */
export default function NoOrgs() {
  const auth = useAuth()
  const navigate = useNavigate()

  // Redirect to /login once logout flips auth to unauth. NoOrgs is
  // outside AuthGate so nothing else observes this state transition.
  useEffect(() => {
    if (auth.status === 'unauth') {
      navigate('/login', { replace: true })
    }
  }, [auth.status, navigate])

  // When provisioning eventually completes (e.g., the user accepts an
  // invite and a follow-up refresh lands the membership), redirect
  // them into the app without a full reload.
  useEffect(() => {
    if (auth.status === 'authed' && auth.orgs.length > 0) {
      navigate('/orgs/' + auth.orgs[0].id, { replace: true })
    }
  }, [auth.status, auth.orgs, navigate])

  const policy = auth.me?.join_policy
  const accountLabel = auth.me?.display_name || auth.me?.email || 'this account'

  // ---- Copy selection ----
  // TODO(invite-flow): when org_invitations exists, prepend an
  //   `auth.me.invitations?.length` branch that renders the
  //   accept-invite affordance with the inviter's org name.

  let title: string
  let body: React.ReactNode

  if (policy === 'invite-only') {
    title = 'Invitation required'
    body = (
      <>
        <p className="text-[13px] text-text-tertiary leading-relaxed">
          You&apos;re signed in as{' '}
          <span className="text-text-secondary font-medium">{accountLabel}</span>. This instance is
          configured to require an invitation before granting access.
        </p>
        <p className="text-[13px] text-text-tertiary leading-relaxed">
          Contact your administrator to request an invite, then log out and back in with the invited
          account.
        </p>
      </>
    )
  } else {
    // Catch-all. Shouldn't fire after SKY-345 lands — every other
    // policy auto-provisions on signup. If it does, something went
    // wrong (DB rollback, race we didn't anticipate, manual revoke
    // of every membership). Offer the user a way out without making
    // promises we can't keep.
    title = "Something's not right"
    body = (
      <>
        <p className="text-[13px] text-text-tertiary leading-relaxed">
          You&apos;re signed in as{' '}
          <span className="text-text-secondary font-medium">{accountLabel}</span>, but you
          don&apos;t appear to be a member of any organization on this deployment.
        </p>
        <p className="text-[13px] text-text-tertiary leading-relaxed">
          Try refreshing. If the problem persists, log out and back in, or contact support.
        </p>
      </>
    )
  }

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <div className="w-full max-w-md backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]">
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">{title}</h1>
          {body}
        </div>

        <div className="flex gap-3">
          <button
            type="button"
            onClick={() => void auth.refresh()}
            className="flex-1 bg-white/50 hover:bg-white/80 border border-border-subtle text-text-secondary font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            Refresh
          </button>
          <button
            type="button"
            onClick={() => void auth.logout()}
            className="flex-1 bg-accent hover:bg-accent/90 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            Log out
          </button>
        </div>
      </div>
    </div>
  )
}
