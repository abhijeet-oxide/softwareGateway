import { Alert, Button, Card, Col, Descriptions, Row, Space, Tag, Typography } from 'antd'
import { useIdentity } from '../auth/permissions'
import { identityClaims, isSignedIn, signOut } from '../auth/session'
import { Value } from '../components/value'
import { PageHeader } from '../components/layout'
import { AppearanceSettings, InlineNotice, StatusPill } from '../uikit'

/**
 * Who the person signed in is, and what that gets them.
 *
 * Split out of Settings, which had grown into two pages sharing a scroll: a
 * database driver and a worker fleet on the same screen as somebody's own name
 * and the only way to sign out. They answer different questions and are opened
 * for different reasons - one of them by clicking your own name in the
 * navigation, which had led to a page about background workers.
 *
 * Everything here is about the reader. Nothing here is about the deployment.
 */
export default function Profile() {
  const { who, loading } = useIdentity()
  /* The Coordinator reports what it verified: a subject, and what that subject
     may do. It cannot report a name, because ZITADEL's ACCESS token does not
     carry one - a name is in the ID token, which is the client's to read. So
     the verified answer wins where it exists and this fills the rest. */
  const claims = identityClaims()
  const name = who?.name || claims.name
  const email = who?.email || claims.email

  const anonymous = Boolean(who && !who.authenticated)
  const productRoles = Object.entries(who?.productRoles ?? {})

  return (
    <>
      <PageHeader
        extra={
          isSignedIn() ? (
            // Danger, and on its own, because it is the one control on this
            // page that ends something. It lives here rather than in the
            // navigation for the same reason: signing out is deliberate.
            <Button danger onClick={() => void signOut()}>
              Sign out
            </Button>
          ) : undefined
        }
      />

      <Row gutter={[16, 16]}>
        <Col xs={24} xl={14}>
          <Card title="Identity" loading={loading}>
            {anonymous ? (
              <Alert
                type="info"
                showIcon
                message="Authentication is not enabled"
                description={
                  <Space direction="vertical" size={4}>
                    <Typography.Text>
                      This Coordinator accepts every caller as{' '}
                      <Typography.Text code>{who?.subject}</Typography.Text> with full
                      permissions. The only control protecting it is network isolation.
                    </Typography.Text>
                    <Typography.Text type="secondary">
                      Everything this interface shows and does is already asked through a
                      permission check, so switching authentication on changes what people
                      can do without changing any page.
                    </Typography.Text>
                  </Space>
                }
              />
            ) : (
              <Descriptions column={1} size="small">
                <Descriptions.Item label="Name">
                  {/* The identity provider does not always assert a name, and a
                      blank line is worse than saying so. */}
                  <Value>{name}</Value>
                </Descriptions.Item>
                <Descriptions.Item label="Email"><Value>{email}</Value></Descriptions.Item>
                {claims.preferredUsername ? (
                  <Descriptions.Item label="Username">
                    <Value>{claims.preferredUsername}</Value>
                  </Descriptions.Item>
                ) : null}
                <Descriptions.Item label="Signed in with">
                  {methodLabel(who?.method)}
                </Descriptions.Item>
                <Descriptions.Item label="Tenant">{who?.tenant || 'All tenants'}</Descriptions.Item>
                <Descriptions.Item label="Account id">
                  {/* Last, and named for what it is. It was the only thing this
                      page showed, under the label "Signed in as", which made a
                      person's own screen introduce them as a number. */}
                  <Typography.Text type="secondary" copyable={Boolean(who?.subject)}>
                    {who?.subject}
                  </Typography.Text>
                </Descriptions.Item>
              </Descriptions>
            )}
          </Card>
        </Col>

        <Col xs={24} xl={10}>
          <Card title="Appearance">
            <AppearanceSettings />
          </Card>
        </Col>

        <Col xs={24}>
          <Card title="Access">
            <Descriptions column={1} size="small">
              <Descriptions.Item label="Tenant roles">
                {/* The two tiers are shown apart because they mean different
                    things: a tenant role covers products that do not exist yet,
                    a product role names one. Merged, this page could not say
                    which kind somebody held. */}
                {who?.roles?.length ? (
                  <Space size={4} wrap>
                    {who.roles.map((r) => <Tag key={r}>{r}</Tag>)}
                  </Space>
                ) : (
                  <Typography.Text type="secondary">None</Typography.Text>
                )}
              </Descriptions.Item>

              <Descriptions.Item label="Product roles">
                {productRoles.length ? (
                  <Space direction="vertical" size={4}>
                    {productRoles.map(([product, roles]) => (
                      <Space key={product} size={6} wrap>
                        <StatusPill tone="neutral">{product}</StatusPill>
                        {roles.map((r) => <Tag key={r}>{r}</Tag>)}
                      </Space>
                    ))}
                  </Space>
                ) : (
                  <Typography.Text type="secondary">None</Typography.Text>
                )}
              </Descriptions.Item>

              <Descriptions.Item label="Permissions">
                <Space size={4} wrap>
                  {(who?.permissions ?? []).length ? (
                    who?.permissions?.map((p) => (
                      <Tag key={p} color={p === '*' ? 'gold' : undefined}>
                        {p === '*' ? 'everything' : p}
                      </Tag>
                    ))
                  ) : (
                    <Typography.Text type="secondary">None</Typography.Text>
                  )}
                </Space>
              </Descriptions.Item>

              <Descriptions.Item label="Visible products">
                {who?.products?.length ? who.products.join(', ') : 'All products'}
              </Descriptions.Item>
            </Descriptions>

            <div style={{ marginTop: 12 }}>
              <InlineNotice tone="info">
                Roles are granted in the identity provider and arrive in the sign-in
                token. Changing one takes effect at the next sign-in.
              </InlineNotice>
            </div>
          </Card>
        </Col>
      </Row>
    </>
  )
}

/**
 * What "oidc" means to somebody who did not configure it.
 *
 * The raw value is the trust path and belongs in an audit record; on this page
 * it was rendered verbatim, so a person who had just clicked a Microsoft button
 * was told their method was `oidc`.
 */
function methodLabel(method: string | undefined): string {
  switch (method) {
    case 'oidc':
      return 'Single sign-on'
    case 'none':
      return 'Not authenticated'
    case '':
    case undefined:
      return 'Unknown'
    default:
      return method
  }
}
