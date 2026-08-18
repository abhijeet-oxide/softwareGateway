import { useMemo, useState } from 'react'
import {
  App, Button, Card, Col, Descriptions, Modal, Row, Space, Table, Tabs, Tag, Tooltip, Typography,
} from 'antd'
import {
  CloudDownloadOutlined, FileOutlined, PictureOutlined, ReloadOutlined,
} from '@ant-design/icons'
import { Link, useNavigate, useParams } from 'react-router-dom'
import {
  useArtifacts, useInspectPackage, usePackage, useProduct, useRunDownload,
} from '../api/queries'
import { useCan } from '../auth/permissions'
import {
  deriveLifecycle, deriveLocations, deriveStatus, isLive, verification, version,
} from '../domain/derive'
import { bytes, formatBytes, formatCount, UNKNOWN } from '../domain/format'
import {
  LocationChip, RepoLink, StatusBadge, TimeAgo, VerificationBadge,
} from '../components/chips'
import {
  ErrorState, LifecycleIndicator, PageHeader, SavedPanel,
} from '../components/layout'
import { mono } from '../theme'
import type { Artifact } from '../api/types'

/**
 * Page 3 — Software (release detail).
 *
 * Answers: everything about this one release, and what can I do with it?
 *
 * Two rules bite hardest here:
 *   - the release downloads WHOLE. The contents are shown and nothing in them
 *     is selectable, because cherry-picking a release is not a thing the
 *     system does.
 *   - "Saved (already present)" lives HERE, not on Home, and is labelled an
 *     estimate before a download and a measurement after.
 */

/** Groups the artifact tree into the three kinds a user thinks in. */
function classify(a: Artifact): 'Images' | 'Helm Charts' | 'Files' {
  const type = `${a.artifactType ?? ''} ${a.mediaType}`.toLowerCase()
  if (type.includes('helm')) return 'Helm Charts'
  if (type.includes('image') || a.platform) return 'Images'
  return 'Files'
}

export default function SoftwareDetail() {
  const { product: productName, reference } = useParams()
  const navigate = useNavigate()
  const { message } = App.useApp()

  const product = useProduct(productName)
  const pkg = usePackage(productName, reference)
  const artifacts = useArtifacts(productName, reference)
  const inspect = useInspectPackage(productName!, reference!)
  const runDownload = useRunDownload(productName!)

  const mayOperate = useCan('operate', { product: productName })
  const [confirming, setConfirming] = useState(false)

  const groups = useMemo(() => {
    const out = {
      Images: { count: 0, bytes: 0 },
      'Helm Charts': { count: 0, bytes: 0 },
      Files: { count: 0, bytes: 0 },
    }
    for (const a of artifacts.data?.artifacts ?? []) {
      const kind = classify(a)
      out[kind].count += 1
      out[kind].bytes += bytes(a.sizeBytes) ?? 0
    }
    return out
  }, [artifacts.data])

  if (pkg.isError) {
    return (
      <>
        <PageHeader title="Software" description="Release detail" />
        <ErrorState error={pkg.error} retry={() => void pkg.refetch()} />
      </>
    )
  }

  const p = pkg.data
  const prod = product.data
  const status = p ? deriveStatus(p, prod) : undefined
  const live = (p?.transfers ?? []).find((t) => isLive(t.state))

  const download = async () => {
    try {
      const result = await runDownload.mutateAsync({ tags: [p!.tag] })
      setConfirming(false)
      message.success(
        result.created?.length
          ? `Download started. It will appear under Downloads.`
          : `This release was already requested; the existing download continues.`,
      )
      navigate('/downloads')
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The download could not be started.')
    }
  }

  const savedBytes = live?.id ? undefined : undefined
  const notExpanded = p && !p.expandedAt

  return (
    <>
      <PageHeader
        title={p ? `${prod?.displayName || productName} ${version(p)}` : 'Loading…'}
        description="One release, its contents, and what can be done with it"
        meta={
          p && (
            <Space>
              <StatusBadge status={status!} />
              <VerificationBadge state={verification(p)} />
            </Space>
          )
        }
        extra={
          <Space>
            <Link to={`/compare?product=${encodeURIComponent(productName!)}&from=${encodeURIComponent(p?.tag ?? '')}`}>
              <Button>Compare</Button>
            </Link>
            {live ? (
              <Link to={`/downloads/${live.id}`}>
                <Button type="primary">View download</Button>
              </Link>
            ) : (
              <Tooltip
                title={
                  mayOperate
                    ? 'Downloads the whole release into the internal repositories and configures the mirror OpenShift pulls from.'
                    : 'You do not have permission to start a download.'
                }
              >
                <Button
                  type="primary"
                  icon={<CloudDownloadOutlined />}
                  disabled={!mayOperate || !p}
                  onClick={() => setConfirming(true)}
                >
                  Download
                </Button>
              </Tooltip>
            )}
          </Space>
        }
      />

      <Row gutter={[16, 16]}>
        <Col span={24}>
          <Card>
            <LifecycleIndicator steps={p ? deriveLifecycle(p, prod) : []} size="default" />
          </Card>
        </Col>

        <Col xs={24} xl={14}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card title="Release" loading={pkg.isLoading}>
              <Descriptions column={2} size="small">
                <Descriptions.Item label="Vendor">
                  {prod?.sources?.[0]?.vendor || prod?.sources?.[0]?.name || UNKNOWN}
                </Descriptions.Item>
                <Descriptions.Item label="Version">
                  <span style={{ fontFamily: mono }}>{p ? version(p) : UNKNOWN}</span>
                </Descriptions.Item>
                <Descriptions.Item label="Published">
                  {p?.publishedAt ? <TimeAgo at={p.publishedAt} /> : (
                    <Tooltip title="The publisher set no build date, which the OCI specification permits.">
                      <Typography.Text type="secondary">{UNKNOWN}</Typography.Text>
                    </Tooltip>
                  )}
                </Descriptions.Item>
                <Descriptions.Item label="Discovered"><TimeAgo at={p?.discoveredAt} /></Descriptions.Item>
                <Descriptions.Item label="Artifacts">{formatCount(p?.artifactCount)}</Descriptions.Item>
                <Descriptions.Item label="Total size">
                  {p?.totalBytes ? formatBytes(p.totalBytes) : (
                    <Tooltip title="The manifest tree has not been walked yet, so the size underneath is not known. Inspect the release to measure it.">
                      <Typography.Text type="secondary">{UNKNOWN}</Typography.Text>
                    </Tooltip>
                  )}
                </Descriptions.Item>
                <Descriptions.Item label="Digest" span={2}>
                  <Typography.Text copyable style={{ fontFamily: mono, fontSize: 12 }}>
                    {p?.manifestDigest ?? UNKNOWN}
                  </Typography.Text>
                </Descriptions.Item>
              </Descriptions>

              {notExpanded && (
                <Button
                  size="small"
                  icon={<ReloadOutlined />}
                  loading={inspect.isPending}
                  disabled={!mayOperate}
                  onClick={() => inspect.mutate()}
                  style={{ marginTop: 8 }}
                >
                  Measure this release
                </Button>
              )}
            </Card>

            <Card
              title="Contents"
              extra={
                <Typography.Text type="secondary">
                  {formatCount(p?.artifactCount)} artifacts
                  {p?.totalBytes ? ` · ${formatBytes(p.totalBytes)}` : ''}
                </Typography.Text>
              }
              loading={artifacts.isLoading}
            >
              <Row gutter={16} style={{ marginBottom: 12 }}>
                {(['Images', 'Helm Charts', 'Files'] as const).map((kind) => (
                  <Col span={8} key={kind}>
                    <Card size="small">
                      <Space direction="vertical" size={0}>
                        <Space size={6}>
                          {kind === 'Images' ? <PictureOutlined /> : <FileOutlined />}
                          <Typography.Text type="secondary">{kind}</Typography.Text>
                        </Space>
                        <Typography.Title level={4} style={{ margin: 0 }}>
                          {groups[kind].count}
                        </Typography.Title>
                        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                          {groups[kind].bytes ? formatBytes(groups[kind].bytes) : UNKNOWN}
                        </Typography.Text>
                      </Space>
                    </Card>
                  </Col>
                ))}
              </Row>

              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                A release is downloaded whole — individual artifacts are not selectable.
              </Typography.Text>

              <Tabs
                size="small"
                style={{ marginTop: 8 }}
                items={(['Images', 'Helm Charts', 'Files'] as const).map((kind) => ({
                  key: kind,
                  label: `${kind} (${groups[kind].count})`,
                  children: (
                    <Table
                      size="small"
                      dataSource={(artifacts.data?.artifacts ?? []).filter((a) => classify(a) === kind)}
                      rowKey={(a) => a.artifactId}
                      pagination={{ pageSize: 8, hideOnSinglePage: true }}
                      scroll={{ x: 600 }}
                      columns={[
                        { title: 'Name', render: (_, a) => <span style={{ fontFamily: mono, fontSize: 12 }}>{a.tag || a.artifactType || kind}</span> },
                        { title: 'Platform', width: 120, render: (_, a) => a.platform || UNKNOWN },
                        { title: 'Size', width: 100, align: 'right', render: (_, a) => formatBytes(a.sizeBytes) },
                        {
                          title: 'Digest',
                          width: 200,
                          render: (_, a) => (
                            <Typography.Text copyable={{ text: a.digest }} style={{ fontFamily: mono, fontSize: 11 }}>
                              {a.digest.slice(0, 19)}…
                            </Typography.Text>
                          ),
                        },
                      ]}
                    />
                  ),
                }))}
              />
            </Card>
          </Space>
        </Col>

        <Col xs={24} xl={10}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card title="Saved (already present)">
              {savedBytes !== undefined ? (
                <SavedPanel
                  savedBytes={formatBytes(savedBytes)}
                  totalBytes={p?.totalBytes ? formatBytes(p.totalBytes) : undefined}
                />
              ) : (
                // No number at all rather than a number-shaped dash: how much
                // of a release the destination already holds cannot be known
                // without asking it, and a download is what asks.
                <Typography.Text type="secondary" style={{ fontSize: 13 }}>
                  Existing artifacts are detected and skipped, so a download moves considerably less
                  than the release weighs. How much is already present is measured during the
                  download and reported there — it cannot be known before one runs.
                </Typography.Text>
              )}
            </Card>

            <Card title="Verification">
              <Space direction="vertical" size={8} style={{ width: '100%' }}>
                <VerificationBadge state={p ? verification(p) : 'UNKNOWN'} />
                {(p?.related ?? []).filter((r) => r.role === 'SIGNATURE').map((sig) => (
                  <Descriptions key={sig.digest} column={1} size="small">
                    <Descriptions.Item label="Signature type">{sig.blobMediaType || sig.mediaType || UNKNOWN}</Descriptions.Item>
                    <Descriptions.Item label="Confirmed"><TimeAgo at={sig.resolvedAt} /></Descriptions.Item>
                  </Descriptions>
                ))}
                {p && verification(p) === 'UNKNOWN' && (
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                    This source does not publish signatures in a layout we can discover. That is not the
                    same as the release being unsigned.
                  </Typography.Text>
                )}
              </Space>
            </Card>

            <Card title="Locations">
              <Space direction="vertical" size={10} style={{ width: '100%' }}>
                <LocationChip locations={p ? deriveLocations(p, prod) : []} />
                {(prod?.targets ?? []).map((t) => (
                  <div key={t.name}>
                    <Space size={8}>
                      <Typography.Text>{t.name}</Typography.Text>
                      {t.environment && <Tag color={t.environment === 'production' ? 'success' : 'default'}>{t.environment}</Tag>}
                    </Space>
                    <div>
                      <RepoLink url={t.registry ? `https://${t.registry.replace(/^https?:\/\//, '')}/${t.repository ?? ''}` : undefined} />
                    </div>
                  </div>
                ))}
              </Space>
            </Card>
          </Space>
        </Col>
      </Row>

      <Modal
        open={confirming}
        title={`Download ${prod?.displayName || productName} ${p ? version(p) : ''}?`}
        okText="Download this release"
        confirmLoading={runDownload.isPending}
        onOk={() => void download()}
        onCancel={() => setConfirming(false)}
      >
        <Typography.Paragraph>
          The whole release is brought into the internal repositories, and the mirror that OpenShift
          pulls from is configured as part of the same operation. Artifacts already present are
          detected and skipped.
        </Typography.Paragraph>
        <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
          {p?.totalBytes
            ? `Up to ${formatBytes(p.totalBytes)} across ${formatCount(p.artifactCount)} artifacts.`
            : `${formatCount(p?.artifactCount)} artifacts. The total size has not been measured yet.`}
        </Typography.Paragraph>
      </Modal>
    </>
  )
}
