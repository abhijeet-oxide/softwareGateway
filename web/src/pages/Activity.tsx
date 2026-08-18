import { useState } from 'react'
import { Button, Card, DatePicker, Descriptions, Drawer, Select, Space, Table, Tag, Typography } from 'antd'
import { DownloadOutlined } from '@ant-design/icons'
import { Link, useSearchParams } from 'react-router-dom'
import { useAuditEvents, useProducts } from '../api/queries'
import { TimeAgo } from '../components/chips'
import { NA, Value } from '../components/value'
import { EmptyStateCard, ErrorState, PageHeader } from '../components/layout'
import { mono } from '../theme'
import type { AuditEvent } from '../api/types'

/**
 * Page 8 — Activity.
 *
 * Answers: what happened, when, and who did it?
 *
 * Filters compose into the URL, so a filtered view can be pasted to a
 * colleague. Event types are rendered into the product's voice by a lookup
 * table rather than by mangling the identifier — a new event type should read
 * as itself rather than as a guess.
 */

const VOICE: Record<string, (e: AuditEvent) => string> = {
  PackageDiscovered: (e) => `New ${e.product ?? 'software'} version ${e.subjectId ?? ''} discovered`.trim(),
  PackageSuperseded: (e) => `${e.subjectId ?? 'A release'} was replaced by a newer build of the same version`,
  DownloadRunRequested: (e) => `${e.product ?? 'Software'} ${e.subjectId ?? ''} download started`.trim(),
  ConfigDrifted: (e) => `The registry configuration for ${e.subjectId ?? 'a target'} drifted from Git`,
  SyncRequested: (e) => `Mirror sync requested for ${e.subjectId ?? 'a target'}`,
  ReplicationApplied: (e) => `Mirror configuration applied to ${e.subjectId ?? 'a target'}`,
}

function describe(e: AuditEvent): string {
  const voice = VOICE[e.eventType]
  if (voice) return voice(e)
  // Unknown event types read as themselves, spaced out. Better an honest
  // identifier than a sentence invented for an event nobody has described.
  return e.eventType.replace(/([a-z])([A-Z])/g, '$1 $2')
}

export default function Activity() {
  const [params, setParams] = useSearchParams()
  const products = useProducts()
  const [open, setOpen] = useState<AuditEvent>()
  const [page, setPage] = useState(1)
  const [token, setToken] = useState<string>()

  const filters = {
    product: params.get('product') ?? undefined,
    eventType: params.get('eventType') ?? undefined,
    outcome: params.get('outcome') ?? undefined,
    actor: params.get('actor') ?? undefined,
    since: params.get('since') ?? undefined,
    until: params.get('until') ?? undefined,
    pageSize: 25,
    pageToken: token,
  }

  const events = useAuditEvents(filters)

  const update = (key: string, value?: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next)
    setToken(undefined)
    setPage(1)
  }

  const exportJson = () => {
    const blob = new Blob([JSON.stringify(events.data?.auditEvents ?? [], null, 2)],
      { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'activity.json'
    a.click()
    URL.revokeObjectURL(url)
  }

  if (events.isError) {
    return (
      <>
        <PageHeader title="Activity" description="What happened, when, and who did it" />
        <ErrorState error={events.error} retry={() => void events.refetch()} />
      </>
    )
  }

  const rows = events.data?.auditEvents ?? []

  return (
    <>
      <PageHeader
        title="Activity"
        description="What happened, when, and who did it"
        meta={
          <Space wrap>
            <Select
              style={{ minWidth: 150 }}
              placeholder="Any product"
              allowClear
              value={filters.product}
              onChange={(v) => update('product', v)}
              loading={products.isLoading}
              options={(products.data?.products ?? []).map((p) => ({
                value: p.productId, label: p.displayName || p.productId,
              }))}
            />
            <Select
              style={{ minWidth: 170 }}
              placeholder="Any event"
              allowClear
              value={filters.eventType}
              onChange={(v) => update('eventType', v)}
              options={Object.keys(VOICE).map((k) => ({ value: k, label: k.replace(/([a-z])([A-Z])/g, '$1 $2') }))}
            />
            <Select
              style={{ minWidth: 130 }}
              placeholder="Any result"
              allowClear
              value={filters.outcome}
              onChange={(v) => update('outcome', v)}
              options={[
                { value: 'success', label: 'Succeeded' },
                { value: 'failure', label: 'Failed' },
              ]}
            />
            <DatePicker.RangePicker
              onChange={(range) => {
                update('since', range?.[0]?.toISOString())
                update('until', range?.[1]?.toISOString())
              }}
            />
          </Space>
        }
        extra={
          <Button icon={<DownloadOutlined />} onClick={exportJson} disabled={rows.length === 0}>
            Export
          </Button>
        }
      />

      {!events.isLoading && rows.length === 0 ? (
        <EmptyStateCard
          title="Nothing has been recorded for this view"
          explanation="Activity is written as the system discovers, downloads and verifies software. Either nothing has happened yet, or these filters exclude it."
          action={
            <Button onClick={() => { setParams(new URLSearchParams()); setToken(undefined) }}>
              Clear filters
            </Button>
          }
        />
      ) : (
        <Card styles={{ body: { padding: 0 } }}>
          <Table
            loading={events.isLoading}
            dataSource={rows}
            rowKey={(e) => e.id}
            scroll={{ x: 900 }}
            onRow={(e) => ({ onClick: () => setOpen(e), style: { cursor: 'pointer' } })}
            pagination={{
              current: page,
              pageSize: 25,
              total: events.data?.nextPageToken ? page * 25 + 1 : rows.length + (page - 1) * 25,
              showSizeChanger: false,
              onChange: (next) => {
                // Server-side paging: the API hands back the token for the next
                // page rather than a count, so forward paging follows it and
                // going back returns to the start of the filter.
                if (next > page && events.data?.nextPageToken) {
                  setToken(events.data.nextPageToken)
                  setPage(next)
                } else if (next < page) {
                  setToken(undefined)
                  setPage(1)
                }
              },
            }}
            columns={[
              { title: 'When', fixed: 'left', width: 120, render: (_, e) => <TimeAgo at={e.occurredAt} /> },
              { title: 'What happened', render: (_, e) => describe(e) },
              {
                title: 'Subject',
                width: 160,
                render: (_, e) =>
                  // Only a PACKAGE subject has a release page. A target or a
                  // rule would produce a link that resolves to nothing.
                  e.product && e.subjectId && e.subjectKind === 'package' ? (
                    <Link
                      to={`/software?product=${encodeURIComponent(e.product)}&tag=${encodeURIComponent(e.subjectId)}`}
                      style={{ fontFamily: mono, fontSize: 12 }}
                      onClick={(ev) => ev.stopPropagation()}
                    >
                      {e.product} {e.subjectId}
                    </Link>
                  ) : e.subjectId ? (
                    <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>
                      {e.subjectId}
                    </Typography.Text>
                  ) : (
                    <NA />
                  ),
              },
              {
                title: 'Result',
                width: 110,
                render: (_, e) =>
                  e.outcome === 'failure' ? <Tag color="red">Failed</Tag> : <Tag color="green">Succeeded</Tag>,
              },
              {
                title: 'Who',
                width: 150,
                render: (_, e) => (
                  <Space size={4}>
                    <span>{e.actor}</span>
                    <Tag>{e.actorKind}</Tag>
                  </Space>
                ),
              },
            ]}
          />
        </Card>
      )}

      <Drawer open={Boolean(open)} onClose={() => setOpen(undefined)} width={560} title={open && describe(open)}>
        {open && (
          <Descriptions column={1} size="small" bordered>
            <Descriptions.Item label="Event type">{open.eventType}</Descriptions.Item>
            <Descriptions.Item label="Occurred">{open.occurredAt}</Descriptions.Item>
            <Descriptions.Item label="Actor">{open.actor} ({open.actorKind})</Descriptions.Item>
            <Descriptions.Item label="Outcome">{open.outcome}</Descriptions.Item>
            <Descriptions.Item label="Product"><Value>{open.product}</Value></Descriptions.Item>
            <Descriptions.Item label="Subject">
              <Value>{open.subjectKind ? `${open.subjectKind} ${open.subjectId ?? ''}` : null}</Value>
            </Descriptions.Item>
            <Descriptions.Item label="Request ID">
              <span style={{ fontFamily: mono, fontSize: 11 }}><Value>{open.requestId}</Value></span>
            </Descriptions.Item>
            <Descriptions.Item label="Detail">
              {open.detail ? (
                <pre style={{ fontFamily: mono, fontSize: 11, margin: 0, whiteSpace: 'pre-wrap' }}>
                  {JSON.stringify(open.detail, null, 2)}
                </pre>
              ) : <NA />}
            </Descriptions.Item>
          </Descriptions>
        )}
      </Drawer>
    </>
  )
}
