import { useState, useEffect, useCallback } from 'react'
import { PieChart, Pie, Cell, ResponsiveContainer, AreaChart, Area, XAxis, YAxis, Tooltip } from 'recharts'
import PRCard from '../components/PRCard'

export interface PRSummary {
  number: number
  title: string
  repo: string
  author: string
  state: string
  draft: boolean
  labels: string[]
  created_at: string
  updated_at: string
  html_url: string
}

interface Stats {
  merged: number
  closed: number
  awaiting: number
  draft: number
  reviews_given: number
  reviews_received: number
  merged_over_time: { week: string; count: number }[]
}

export default function PRDashboard() {
  const [prs, setPrs] = useState<PRSummary[]>([])
  const [stats, setStats] = useState<Stats | null>(null)
  const [loading, setLoading] = useState(true)

  const fetchAll = useCallback(async () => {
    setLoading(true)
    try {
      const [prsRes, statsRes] = await Promise.all([
        fetch('/api/dashboard/prs').then((r) => r.json()),
        fetch('/api/dashboard/stats').then((r) => r.json()),
      ])
      setPrs(prsRes)
      setStats(statsRes)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    fetchAll()
  }, [fetchAll])

  // Auto-refresh every 2 min while visible
  useEffect(() => {
    const interval = setInterval(fetchAll, 120000)
    const handleVis = () => {
      if (document.visibilityState === 'visible') fetchAll()
    }
    document.addEventListener('visibilitychange', handleVis)
    return () => {
      clearInterval(interval)
      document.removeEventListener('visibilitychange', handleVis)
    }
  }, [fetchAll])

  const draftPRs = prs.filter((pr) => pr.draft)
  const readyPRs = prs.filter((pr) => !pr.draft)

  return (
    <div className="max-w-5xl mx-auto">
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-xl font-semibold text-text-primary">Pull Requests</h1>
        <button
          onClick={fetchAll}
          disabled={loading}
          className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors disabled:opacity-50"
        >
          {loading ? 'Refreshing...' : 'Refresh'}
        </button>
      </div>

      {/* Chart strip */}
      <div className="grid grid-cols-4 gap-4 mb-8">
        <ChartCard title="Status">
          {stats ? <StatusDonut stats={stats} /> : <SkeletonDonut />}
        </ChartCard>
        <ChartCard title="Merged this month">
          {stats ? <MergedTimeline data={stats.merged_over_time || []} /> : <SkeletonChart />}
        </ChartCard>
        <ChartCard title="Review balance">
          {stats ? <ReviewBalance given={stats.reviews_given} received={stats.reviews_received} /> : <SkeletonBar />}
        </ChartCard>
        <ChartCard title="30-day totals">
          {stats ? <TotalsSummary stats={stats} /> : <SkeletonTotals />}
        </ChartCard>
      </div>

      {/* PR columns */}
      <div className="grid grid-cols-2 gap-6">
        <div>
          <h2 className="text-[13px] font-medium text-text-secondary mb-3 px-1">
            Ready for review
            <span className="ml-2 text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5 text-[11px]">
              {readyPRs.length}
            </span>
          </h2>
          <div className="space-y-3">
            {readyPRs.length === 0 ? (
              <p className="text-[12px] text-text-tertiary text-center py-8">No open PRs</p>
            ) : (
              readyPRs.map((pr) => <PRCard key={`${pr.repo}-${pr.number}`} pr={pr} />)
            )}
          </div>
        </div>
        <div>
          <h2 className="text-[13px] font-medium text-text-secondary mb-3 px-1">
            Drafts
            <span className="ml-2 text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5 text-[11px]">
              {draftPRs.length}
            </span>
          </h2>
          <div className="space-y-3">
            {draftPRs.length === 0 ? (
              <p className="text-[12px] text-text-tertiary text-center py-8">No drafts</p>
            ) : (
              draftPRs.map((pr) => <PRCard key={`${pr.repo}-${pr.number}`} pr={pr} />)
            )}
          </div>
        </div>
      </div>
    </div>
  )
}

function ChartCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="bg-surface-raised backdrop-blur-xl border border-border-glass rounded-2xl p-4 shadow-sm shadow-black/[0.03]">
      <h3 className="text-[11px] font-medium text-text-tertiary mb-3">{title}</h3>
      {children}
    </div>
  )
}

const COLORS = {
  merged: 'var(--color-claim)',
  closed: 'var(--color-dismiss)',
  awaiting: 'var(--color-accent)',
  draft: 'var(--color-text-tertiary)',
}

function StatusDonut({ stats }: { stats: Stats }) {
  const data = [
    { name: 'Merged', value: stats.merged, color: COLORS.merged },
    { name: 'Closed', value: stats.closed, color: COLORS.closed },
    { name: 'Awaiting', value: stats.awaiting, color: COLORS.awaiting },
    { name: 'Draft', value: stats.draft, color: COLORS.draft },
  ].filter((d) => d.value > 0)

  const total = data.reduce((sum, d) => sum + d.value, 0)

  if (total === 0) {
    return <p className="text-[12px] text-text-tertiary text-center py-4">No data</p>
  }

  return (
    <div className="flex items-center gap-3">
      <div className="w-16 h-16">
        <ResponsiveContainer>
          <PieChart>
            <Pie
              data={data}
              cx="50%"
              cy="50%"
              innerRadius={18}
              outerRadius={30}
              dataKey="value"
              stroke="none"
            >
              {data.map((d, i) => (
                <Cell key={i} fill={d.color} />
              ))}
            </Pie>
          </PieChart>
        </ResponsiveContainer>
      </div>
      <div className="space-y-0.5">
        {data.map((d) => (
          <div key={d.name} className="flex items-center gap-1.5 text-[11px]">
            <span className="w-1.5 h-1.5 rounded-full" style={{ background: d.color }} />
            <span className="text-text-tertiary">{d.value} {d.name.toLowerCase()}</span>
          </div>
        ))}
      </div>
    </div>
  )
}

function MergedTimeline({ data }: { data: { week: string; count: number }[] }) {
  if (data.length === 0) {
    return <p className="text-[12px] text-text-tertiary text-center py-4">No data</p>
  }

  const formatted = data.map((d) => ({
    ...d,
    label: new Date(d.week + 'T00:00:00').toLocaleDateString([], { month: 'short', day: 'numeric' }),
  }))

  return (
    <div className="h-16">
      <ResponsiveContainer>
        <AreaChart data={formatted}>
          <defs>
            <linearGradient id="mergedGrad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--color-claim)" stopOpacity={0.3} />
              <stop offset="100%" stopColor="var(--color-claim)" stopOpacity={0} />
            </linearGradient>
          </defs>
          <XAxis dataKey="label" hide />
          <YAxis hide />
          <Tooltip
            contentStyle={{
              background: 'rgba(255,255,255,0.8)',
              backdropFilter: 'blur(12px)',
              border: '1px solid rgba(255,255,255,0.7)',
              borderRadius: '8px',
              fontSize: '11px',
              boxShadow: '0 4px 12px rgba(0,0,0,0.05)',
            }}
            formatter={(value: any) => [`${value} PR${value !== 1 ? 's' : ''}`, 'Merged']}
            labelFormatter={(label: any) => `Week of ${label}`}
          />
          <Area
            type="monotone"
            dataKey="count"
            stroke="var(--color-claim)"
            strokeWidth={2}
            fill="url(#mergedGrad)"
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}

function ReviewBalance({ given, received }: { given: number; received: number }) {
  const total = given + received
  if (total === 0) {
    return <p className="text-[12px] text-text-tertiary text-center py-4">No reviews yet</p>
  }

  const givenPct = (given / total) * 100
  const net = given - received
  const label = net > 0 ? `+${net} net given` : net < 0 ? `${net} net received` : 'balanced'

  return (
    <div className="space-y-2">
      <div className="flex h-2.5 rounded-full overflow-hidden bg-black/[0.04]">
        <div
          className="h-full rounded-l-full"
          style={{ width: `${givenPct}%`, background: 'var(--color-delegate)' }}
        />
        <div
          className="h-full rounded-r-full"
          style={{ width: `${100 - givenPct}%`, background: 'var(--color-accent)' }}
        />
      </div>
      <div className="flex justify-between text-[11px]">
        <span className="text-delegate">{given} given</span>
        <span className="text-accent">{received} received</span>
      </div>
      <p className="text-[11px] text-text-tertiary text-center">{label}</p>
    </div>
  )
}

function TotalsSummary({ stats }: { stats: Stats }) {
  const total = stats.merged + stats.closed + stats.awaiting + stats.draft
  return (
    <div className="space-y-2">
      <div className="text-center">
        <span className="text-2xl font-semibold text-text-primary">{total}</span>
        <p className="text-[11px] text-text-tertiary">total PRs</p>
      </div>
      <div className="grid grid-cols-2 gap-x-3 gap-y-1 text-[11px]">
        <div className="flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full bg-claim" />
          <span className="text-text-tertiary">{stats.merged} merged</span>
        </div>
        <div className="flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full bg-dismiss" />
          <span className="text-text-tertiary">{stats.closed} closed</span>
        </div>
        <div className="flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full bg-accent" />
          <span className="text-text-tertiary">{stats.awaiting} open</span>
        </div>
        <div className="flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full" style={{ background: 'var(--color-text-tertiary)' }} />
          <span className="text-text-tertiary">{stats.draft} draft</span>
        </div>
      </div>
    </div>
  )
}

// --- Skeletons ---

const shimmer = 'animate-pulse bg-black/[0.04] rounded'

function SkeletonDonut() {
  return (
    <div className="flex items-center gap-3">
      <div className={`w-16 h-16 rounded-full ${shimmer}`} />
      <div className="space-y-1.5 flex-1">
        <div className={`h-2.5 w-16 ${shimmer}`} />
        <div className={`h-2.5 w-12 ${shimmer}`} />
        <div className={`h-2.5 w-14 ${shimmer}`} />
      </div>
    </div>
  )
}

function SkeletonChart() {
  return (
    <div className="h-16 flex items-end gap-1.5 px-1">
      {[40, 60, 35, 80, 55].map((h, i) => (
        <div key={i} className={`flex-1 ${shimmer}`} style={{ height: `${h}%` }} />
      ))}
    </div>
  )
}

function SkeletonBar() {
  return (
    <div className="space-y-2">
      <div className={`h-2.5 w-full rounded-full ${shimmer}`} />
      <div className="flex justify-between">
        <div className={`h-2.5 w-12 ${shimmer}`} />
        <div className={`h-2.5 w-14 ${shimmer}`} />
      </div>
      <div className={`h-2.5 w-16 mx-auto ${shimmer}`} />
    </div>
  )
}

function SkeletonTotals() {
  return (
    <div className="space-y-2">
      <div className="text-center">
        <div className={`h-7 w-10 mx-auto mb-1 ${shimmer}`} />
        <div className={`h-2.5 w-14 mx-auto ${shimmer}`} />
      </div>
      <div className="grid grid-cols-2 gap-x-3 gap-y-1.5">
        {[1, 2, 3, 4].map((i) => (
          <div key={i} className={`h-2.5 w-16 ${shimmer}`} />
        ))}
      </div>
    </div>
  )
}
