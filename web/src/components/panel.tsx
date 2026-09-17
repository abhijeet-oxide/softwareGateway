import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { Typography } from 'antd'
import { ArrowRightOutlined } from '../icons'
import { c } from '../uikit'

/**
 * The last line of a summary panel: what it adds up to, and where the whole of
 * it lives.
 *
 * # Why the way out is at the BOTTOM
 *
 * A summary is a promise that there is more, and the reader who wants the more
 * has just finished reading the summary - so the link belongs where their eye
 * already is. Putting it in the header, beside the title, offers the exit
 * before the content it is an exit from.
 *
 * The sentence beside it is the panel's conclusion in words. Every one of these
 * panels states something in colour - a green strip, an amber pill - and a
 * conclusion that exists only as a colour is a conclusion half the readers do
 * not get. This is where it is also written down.
 */
export function PanelFoot({
  children,
  to,
  action,
}: {
  /** The panel's conclusion, in a sentence. */
  children?: ReactNode
  to: string
  action: string
}) {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: children ? 'space-between' : 'flex-end',
        gap: 12,
        marginTop: 'auto',
        paddingTop: 12,
        borderTop: `1px solid ${c.border}`,
      }}
    >
      {children && (
        <Typography.Text type="secondary" style={{ fontSize: 11.5, lineHeight: 1.45 }}>
          {children}
        </Typography.Text>
      )}
      <Link to={to} style={{ fontSize: 12, whiteSpace: 'nowrap', flexShrink: 0 }}>
        {action} <ArrowRightOutlined style={{ fontSize: 11 }} />
      </Link>
    </div>
  )
}
