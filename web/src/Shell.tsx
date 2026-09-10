import { useState, type ReactNode } from 'react'
import { Badge, Button, Tooltip } from 'antd'
import {
  BarChartOutlined, BellOutlined, DashboardOutlined, DatabaseOutlined, HistoryOutlined,
  InboxOutlined, PackageOutlined, ProductOutlined, QuestionCircleOutlined,
  ScaleOutlined, SettingOutlined,
} from './icons'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { initialsOf, useIdentity, type Permission } from './auth/permissions'
import { accountBadge } from './auth/badge'
import { accountLabel } from './auth/roles'
import { identityClaims } from './auth/session'
import { useTransferActivity, useVersion, useWorkers } from './api/queries'
import { describeFleet, summariseFleet } from './domain/fleet'
import brand from './brand'
import {
  AppShell, c, ConnectionAlert, ConnectionPill, envHex, SideNav, ThemeToggleButton, TopBar,
  useConnection, withAlpha,
  type NavItem, type NavProfile,
} from './uikit'

// Whether the navigation is collapsed is a per-person preference, not state the
// server has an opinion about, so it lives on the device - the same place the
// sibling tool keeps it, and the same place the theme lives.
const COLLAPSE_KEY = 'gateway.nav.collapsed.v1'

function loadCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSE_KEY) === '1'
  } catch {
    // Private browsing, storage disabled by policy: expanded is the right
    // default and a preference failing to load must never stop a page painting.
    return false
  }
}

/**
 * The application shell.
 *
 * Nine nav items, in this order, and nothing else ever. Detail views are
 * drill-downs, not extra entries (UI brief §3).
 *
 * The chrome itself - the navigation's width and item heights, its hover and
 * active language, the collapse behaviour, the profile card at its foot, the
 * bar's height and how its two ends are arranged - is `SideNav`/`TopBar` from
 * the shared design system, byte-identical to what the sibling tool mounts.
 * What is this application's is only WHICH entries there are and what belongs
 * in its bar.
 *
 * # A navigation is a promise
 *
 * Every entry here says "there is something behind this for you", and for a
 * product owner four of them were lying: Activity, Reports, Repositories and
 * Settings all opened a page whose reads answered 403, and the pages showed
 * empty tables rather than saying so. An empty table is worse than a missing
 * entry - it is a confident statement that nothing has happened in a system
 * they simply cannot see.
 *
 * So each entry names the permission its page needs and is DROPPED for a caller
 * who does not hold it, and the pages behind them refuse themselves as well
 * (`RequirePermission`) so a bookmark or a typed address meets the same answer.
 * The two are separate on purpose: hiding the door is a courtesy, refusing at
 * the door is the behaviour, and neither is the security control - that is the
 * server, on every request.
 */
const NAV: {
  key: string
  icon: ReactNode
  label: string
  /** What the page behind it needs. Absent means every signed-in caller. */
  permission?: Permission
  /**
   * Ask whether the permission is held ANYWHERE rather than tenant-wide.
   *
   * True for a page whose contents the server narrows per product - a product
   * owner sees their own products' releases, downloads and audit events. False
   * for the estate pages, which have no product tier at all and so cannot be
   * narrowed to anything: showing a product owner the Reports entry offers them
   * a page that can only refuse.
   */
  anyScope?: boolean
}[] = [
  { key: '/', icon: <DashboardOutlined />, label: 'Overview' },
  { key: '/products', icon: <ProductOutlined />, label: 'Products', permission: 'product.view', anyScope: true },
  { key: '/packages', icon: <PackageOutlined />, label: 'Packages', permission: 'package.view', anyScope: true },
  {
    key: '/downloads',
    icon: <InboxOutlined />,
    label: 'Downloads',
    permission: 'software_download.view',
    anyScope: true,
  },
  // Hidden for now. Re-enabling it is this block plus `SafetyOutlined` back in
  // the import above - the icon is in the registry, it is just not imported
  // while nothing renders it (the build refuses an unused import).
  // {
  //   key: '/security',
  //   icon: <SafetyOutlined />,
  //   // "Security" rather than "Vulnerabilities": the page answers questions
  //   // about packages and images that have none as readily as about ones that
  //   // do, and a nav entry named after the bad news is one people avoid.
  //   label: 'Security',
  // },
  // The rulebook: every check this organization applies, and what each asserts.
  // Not scoped to a product or a release - it is what WILL be checked, and the
  // person most likely to want it is a vendor who has not shipped yet. It was
  // reachable only from a link inside one release's Compliance tab, which is a
  // page nobody finds if they have not already found a finding.
  { key: '/policies', icon: <ScaleOutlined />, label: 'Policies', permission: 'policy_catalogue.view' },
  {
    key: '/repositories',
    icon: <DatabaseOutlined />,
    label: 'Repositories',
    permission: 'product.view',
    anyScope: true,
  },
  // The audit trail is the one estate resource with a product tier: the server
  // narrows it, so a product reader sees their own products' events.
  { key: '/activity', icon: <HistoryOutlined />, label: 'Activity', permission: 'audit_event.view', anyScope: true },
  { key: '/reports', icon: <BarChartOutlined />, label: 'Reports', permission: 'report.view' },
  // The deployment itself: its version, its dependencies, its worker fleet.
  // An estate page and an administrator's, so it is not offered to anybody
  // whose access is a product.
  { key: '/settings', icon: <SettingOutlined />, label: 'Settings', permission: 'system.view' },
]

/**
 * What the system is doing right now, in one line - WHEN it is doing anything.
 *
 * # Why the settled state is nothing at all
 *
 * It used to be a green dot reading "Downloads completed", stated rather than
 * left blank on the argument that a bar which goes quiet is a bar people learn
 * to ignore. That argument is right about a bar and wrong about this one, for
 * two reasons that only became visible once the bar had more than one thing in
 * it.
 *
 * It is not true. "Downloads completed" is drawn from three counts being zero,
 * and three zeroes are also what an estate that has never downloaded anything
 * looks like - so the sentence claims an outcome for work that was never done.
 *
 * And it is a second green dot. The connection indicator beside it is already
 * a green dot, permanently, and two of them a centimetre apart say two
 * different things in the same shape and colour: one means the service is
 * reachable, the other meant some downloads finished at some point. A reader
 * scanning that corner for a status has to learn which dot is which before
 * either can tell them anything.
 *
 * So this renders only when there is work: running, waiting, or failed. The
 * corner then has exactly one standing dot - whether the service is there -
 * and anything that appears beside it is news.
 */
function ActivityPill({ moving, held, failing, hint }: {
  moving: number
  held: number
  failing: number
  /** What the bar can say about WHY, on hover. */
  hint: string
}) {
  /*
    MOVING and HELD are counted apart, and that is the whole change.

    This said "N downloads running" over a count that included every planned,
    queued and unstarted one. On a fleet that is down - the case somebody most
    needs this bar for - it reported the exact thing they were worried about as
    working, in the brand colour, with a pulse next to it.

    Held work is amber rather than blue: it is not failing, and it is not going
    either, and those are three states rather than two.
  */
  const [tone, text] =
    failing > 0
      ? [c.danger, `${failing} download${failing === 1 ? '' : 's'} failed`]
      : moving > 0
        ? [c.brand, `${moving} download${moving === 1 ? '' : 's'} running`
            + (held > 0 ? `, ${held} waiting` : '')]
        : held > 0
          ? [c.pending, `${held} download${held === 1 ? '' : 's'} waiting to start`]
          : [undefined, undefined]

  // NOTHING IS HAPPENING, so nothing is said. See the note above: the settled
  // sentence was a claim about work that may never have existed, wearing the
  // same green dot as the connection indicator next to it.
  if (tone === undefined || text === undefined) return null

  const running = moving

  return (
    <Link to="/downloads" style={{ textDecoration: 'none' }} title={hint}>
      <span
        style={{
          display: 'inline-flex', alignItems: 'center', gap: 8,
          padding: '4px 12px 4px 10px', borderRadius: 999,
          background: c.surface2, border: `1px solid ${c.border}`,
          fontSize: 12.5, lineHeight: 1.4, color: c.text2, whiteSpace: 'nowrap',
          fontVariantNumeric: 'tabular-nums',
        }}
      >
        <span
          aria-hidden
          style={{
            width: 7, height: 7, borderRadius: '50%', background: tone,
            // The pulse marks work in flight and stops the moment it settles,
            // so movement in this bar always means something is moving.
            //
            // withAlpha rather than two more hex digits: `tone` is a var() now,
            // and `var(--brand)22` is not a colour - the rule would simply be
            // dropped and the ring never appear.
            boxShadow: running > 0 && failing === 0 ? `0 0 0 3px ${withAlpha(tone, 0.13)}` : undefined,
          }}
        />
        {text}
      </span>
    </Link>
  )
}

export function Shell({ children }: { children: ReactNode }) {
  const location = useLocation()
  const navigate = useNavigate()
  const { who, can, canAny } = useIdentity()

  const [collapsed, setCollapsed] = useState(loadCollapsed)
  const version = useVersion()

  /*
    THREE NUMBERS, asked for as three numbers.

    This used to fetch the hundred most recent transfers every few seconds and
    count them here. Every one of those rows carries a dozen aggregates over
    that transfer's jobs, so the request cost what the estate had DONE rather
    than what this bar wanted - and the bar is on every page, so it was issued
    from every open tab for the whole life of a download, against a Coordinator
    already busy leasing and completing jobs. See useTransferActivity.

    The moving/held split still comes from the server, because it is the same
    split domain/fleet makes and the two must not drift: a transfer with a job
    in a worker's hands is moving, one without is not.
  */
  const activity = useTransferActivity()
  const moving = activity.data?.moving ?? 0
  const held = activity.data?.held ?? 0
  const failing = activity.data?.failed ?? 0

  /*
    The fleet, read here because the bar is the one thing on screen from every
    page. "Three downloads running" with no worker running them is the most
    expensive sentence this interface can print, and this is where it printed
    it. See domain/fleet.
  */
  const workerList = useWorkers()
  const fleet = summariseFleet(workerList.data?.workers, workerList.isSuccess)

  /*
    NOTHING is selected on a page the navigation does not list, and that is a
    real state rather than an oversight.

    This used to fall back to '/', so every unlisted route - Security, which is
    reachable from a release and from a listing but has no entry of its own -
    lit the Overview item and titled the bar "Overview". The reader was told
    they were somewhere they were not.
  */
  const selected = NAV.map((n) => n.key)
    .filter((k) => (k === '/' ? location.pathname === '/' : location.pathname.startsWith(k)))
    .sort((a, b) => b.length - a.length)[0]

  // Where you are, for the pages that are reached THROUGH something rather
  // than from the navigation. Longest prefix wins, so /packages/compare is a
  // comparison rather than a package.
  const UNLISTED: [string, string][] = [
    ['/packages/compare', 'Compare releases'],
    ['/security', 'Security'],
    ['/profile', 'Profile'],
  ]
  const section = NAV.find((n) => n.key === selected)?.label
    ?? UNLISTED.filter(([path]) => location.pathname.startsWith(path))
      .sort((a, b) => b[0].length - a[0].length)[0]?.[1]
    ?? brand.appName

  /*
    ONLY WHAT THIS ACCOUNT CAN OPEN.

    Filtered rather than disabled: a navigation is a list of places, and a
    place you may not go is not a place. The page behind each of these refuses
    itself as well - see `RequirePermission` - so this is the courtesy and that
    is the behaviour.

    UNTIL THE ANSWER ARRIVES, everything. Filtering on an identity we have not
    got yet empties the rail on every load and fills it a moment later, which
    reads as an application that has just noticed who you are; and if /whoami
    failed outright it would leave one entry on screen for somebody who holds
    every permission in the system. Not knowing is not a refusal.
  */
  const visible = who
    ? NAV.filter((n) => !n.permission || (n.anyScope ? canAny(n.permission) : can(n.permission)))
    : NAV

  const items: NavItem[] = visible.map((n) => ({
    key: n.key,
    label: n.label,
    icon: n.icon,
    ...(n.key === '/downloads' && failing > 0 ? { badge: failing } : {}),
    onClick: () => navigate(n.key),
  }))

  /*
    The person at the navigation's foot, in the same card the sibling tool uses.

    NAME, not subject. `subject` is the identity provider's opaque id - a long
    number - and it was what this card showed, so the one place in the product
    that says who you are said it as a number. The identity provider does not
    always assert a name, so email is the fallback and the id is the last
    resort rather than the first choice.

    The second line is what the person HOLDS, and it has to cope with both
    tiers: somebody whose access is entirely per-product holds no tenant role
    at all, and this card used to invent "Product Owner" for them.
  */
  // The Coordinator's answer first, because it is the one that was verified.
  // It only has a name when the ACCESS token carried one, which ZITADEL's does
  // not - so in practice this falls through to the ID token, which is where
  // OpenID Connect puts who somebody is. See auth/session identityClaims.
  const claims = identityClaims()
  const navName = who?.name || claims.name || who?.email || claims.email
    || claims.preferredUsername || who?.subject || 'Not signed in'
  const profile: NavProfile = {
    name: navName,
    // Derived HERE rather than by the shared kit, which falls back to the
    // first two characters of the name: that reads "Platform Administrator"
    // as PL while the profile page says PA, and one account looks like two.
    initials: initialsOf(navName),
    /*
      ONE WORD, not twelve.

      This joined every role the account holds with commas: an administrator
      with ten products read as `org-admin, org-member, software-01:
      product-owner, software-02: product-owner, ...`, wrapped into a paragraph
      of raw role identifiers in a two-hundred-pixel rail, under the one line
      on screen whose job is to say who you are.

      Twelve strings is not twelve facts. It is one fact - what this person is
      here - and a list of products, and the list belongs on the profile page
      where there is room to arrange it. See auth/roles.
    */
    sub: accountLabel(who),
    /*
      UNTIL THE ANSWER ARRIVES, no badge, for the reason the navigation shows
      every entry until then: not knowing is not a refusal. A padlock drawn
      over an identity that has simply not loaded yet tells somebody their
      account has no access, a moment before it turns out to be an
      administrator's.
    */
    /*
      WHAT KIND OF ACCOUNT, as a glyph on the avatar.

      The line under the name already says it in words, and words in 10.5px
      grey type are the least glanceable thing in the rail: Admin, Operator,
      Security, Reader and User sit in the same place and differ by a few
      letters, for the one fact that decides whether every control on screen is
      available to you. Collapsed, the words are not there at all and the badge
      is the only thing left saying it.

      From auth/badge, which the profile page's standing chip also reads - one
      picture for one meaning, in both places that name it. It is undefined
      until /whoami answers, for the reason the navigation above shows every
      entry until then.
    */
    badge: accountBadge(who),
    badgeLabel: who ? accountLabel(who) : undefined,
    active: location.pathname.startsWith('/profile'),
    onClick: () => navigate('/profile'),
  }

  return (
    <AppShell
      nav={
        <SideNav
          brand={brand}
          items={items}
          activeKey={selected}
          collapsed={collapsed}
          onToggleCollapse={() => {
            const next = !collapsed
            setCollapsed(next)
            try {
              localStorage.setItem(COLLAPSE_KEY, next ? '1' : '0')
            } catch {
              // The session keeps the choice; it just will not be remembered.
            }
          }}
          profile={profile}
          footer={<DeploymentNote version={version.data?.version} />}
        />
      }
      header={
        /*
          The top bar carries WHERE YOU ARE and WHAT THE SYSTEM IS DOING.

          It was an empty white band with two icons pushed against the right
          edge - fourteen hundred pixels of nothing above every page. The two
          facts now in it were both already computed and both hidden: the
          section name existed only as a highlighted nav item on the other side
          of the window, and the number of downloads currently running lived
          inside the tooltip of a bell, which is to say nowhere. An operations
          console should say what it is doing without being asked.
        */
        <TopBar
          title={section}
          right={
            <>
              {/*
                NOT ASKED AT ALL by a caller who may not read.

                The pill itself now says nothing when nothing is happening -
                see ActivityPill - so this guard is no longer what stops a
                reader being told "Downloads completed" about an estate they
                cannot see. It stays because the counts behind it are a read
                they are refused: without it the shell polls a refusal every
                ten seconds for the whole of their session.
              */}
              {canAny('software_download.view') && (
                <ActivityPill
                  moving={moving}
                  held={held}
                  failing={failing}
                  hint={describeFleet(fleet)}
                />
              )}
              {/*
                WHETHER THE SERVICE IS THERE, permanently on screen.

                A dot while everything works, and a word the moment it does
                not. It is here rather than beside the version at the
                navigation's foot because this is where a person's eye already
                goes when a screen stops answering - the same corner as the
                activity, the theme and the account - and because a fact that
                changes belongs beside the other facts that change.

                It names the product rather than the process. "Coordinator" is
                this system's word for its own server; the person reading a bar
                at half past four wants to know whether Software Gateway is
                working.
              */}
              <ConnectionPill service={brand.appName} />
              {/*
                Light and dark, in the one place a person looks for it. The
                control comes from the shared kit, so both tools put the same
                button in the same corner.
              */}
              <ThemeToggleButton />
              <Tooltip title="Documentation">
                <Button type="text" icon={<QuestionCircleOutlined />} aria-label="Help" />
              </Tooltip>
              <Tooltip
                title={failing ? `${failing} download${failing === 1 ? '' : 's'} failed` : 'No notifications'}
              >
                <Badge count={failing} size="small">
                  <Button type="text" icon={<BellOutlined />} aria-label="Notifications" />
                </Badge>
              </Tooltip>
            </>
          }
        />
      }
    >
      {children}
      {/*
        THE OUTAGE CARD, mounted once for the whole application.

        Inside the shell rather than at the root, which is deliberate: the boot
        gate owns the screen until the service has answered once, and a corner
        card explaining an outage on top of a full page explaining the same
        outage is two voices saying one thing. From here on it is the only
        voice, and it never takes the page away from whoever is using it.
      */}
      <ConnectionAlert service={brand.appName} workNote={<UnsavedWorkNote />} />
    </AppShell>
  )
}

/**
 * The line under an outage that answers the only question somebody in the
 * middle of something actually has.
 *
 * It is rendered only when there IS something to lose. A blanket reassurance
 * shown on a dashboard nobody is typing into is noise, and worse, it teaches
 * people to stop reading the line for the one time it matters. `holdWork` is
 * how a screen declares itself - see `useHeldWork` in the shared kit - and a
 * form calls it while it has changes that have not been saved.
 */
function UnsavedWorkNote() {
  // Read from the snapshot, so declaring work re-renders this: a note that
  // appeared on the next unrelated render would appear seconds late, or never.
  const { heldWork } = useConnection()
  if (heldWork === 0) return null
  // One clause, because it is appended to the card's sentence rather than
  // given a paragraph of its own. "Saving is possible again as soon as the
  // connection is back" is already what the sentence in front of it says.
  return <>Unsaved changes on this screen are kept.</>
}

/**
 * Which installation this is, at the navigation's foot.
 *
 * A support conversation or a screenshot that does not say which Coordinator
 * it came from costs a round trip to establish, every time. The dot carries the
 * environment's own colour - deliberately not a status colour, because a
 * healthy production deployment is not a warning.
 */
function DeploymentNote({ version }: { version?: string }) {
  if (!version) return null
  return (
    <div className="ui-nav-note">
      <span
        style={{
          width: 6,
          height: 6,
          borderRadius: 3,
          flexShrink: 0,
          background: envHex('production'),
        }}
      />
      Coordinator {version}
    </div>
  )
}
