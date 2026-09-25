"""Single-pass simulated trader, meant to be run by cron every minute or two through market hours."""
import argparse
import fcntl
import json
import sys
from datetime import datetime, timedelta, time as dtime, timezone

from logging_config import setup_logger
from sim import config as cfg
from sim import market
from sim.exits import order_flow, volume_climax, exit_reason
from sim.market import ET
from sim.selection import select_candidates
from sim.store import SimStore

logger = setup_logger(name="sim_trader", log_file="sim.log")


def _dump(obj):
    return obj.model_dump() if hasattr(obj, "model_dump") else obj


def _pct(a, b):
    return round((a - b) / b * 100, 3) if a and b else None


def _snapshots(symbols: list) -> dict:
    try:
        return market.snapshots(symbols)
    except Exception as e:
        logger.warning(f"Batch snapshot failed ({e}); retrying per symbol")
    out = {}
    for s in symbols:
        try:
            out.update(market.snapshots([s]))
        except Exception:
            pass
    return out


def _premarket(symbols: list, session) -> dict:
    start = datetime.combine(session.day, dtime(4, 0), tzinfo=ET)
    try:
        bars = market.minute_bars(symbols, start, session.open)
    except Exception as e:
        logger.warning(f"Premarket bars failed: {e}")
        return {}
    return {s: {"volume": sum(b.volume for b in bs), "high": max(b.high for b in bs), "bars": len(bs)}
            for s, bs in bars.items() if bs}


def enter_positions(store: SimStore, session, now: datetime):
    prev = market.previous_session(session.day)
    candidates, threshold = select_candidates(session, prev, now, store.open_tickers())
    logger.info(f"{len(candidates)} candidates (strength threshold {threshold:.3f}, news since {prev.close:%a %H:%M})")
    if not candidates:
        return

    symbols = [c["ticker"] for c in candidates]
    snaps = _snapshots(symbols)
    premarket = _premarket(symbols, session)
    entered = 0

    for c in candidates:
        t = c["ticker"]

        def reject(reason):
            store.log_candidate(session.day, c, "rejected", reason)
            logger.info(f"  skip ${t:<6} {c['source']:<5} {reason}")

        if entered >= cfg.MAX_NEW_POSITIONS:
            reject("max_new_positions")
            continue
        asset = market.asset_info(t)
        if not asset or not asset["tradable"] or asset["exchange"] not in market.TRADABLE_EXCHANGES:
            reject(f"not_tradable ({asset['exchange'] if asset else 'unknown'})")
            continue
        snap = snaps.get(t)
        quote = snap.latest_quote if snap else None
        if not quote or not quote.ask_price or not quote.bid_price:
            reject("no_quote")
            continue
        quote_age = (now - quote.timestamp).total_seconds()
        if quote_age > cfg.MAX_QUOTE_AGE_SEC:
            reject(f"stale_quote ({quote_age:.0f}s)")
            continue
        ask, bid = quote.ask_price, quote.bid_price
        spread_pct = (ask - bid) / ((ask + bid) / 2) * 100
        if ask < cfg.MIN_PRICE:
            reject(f"price {ask:.2f} < {cfg.MIN_PRICE}")
            continue
        if spread_pct > cfg.MAX_SPREAD_PCT:
            reject(f"spread {spread_pct:.2f}%")
            continue
        prev_close = snap.previous_daily_bar.close if snap.previous_daily_bar else None
        today_bar = snap.daily_bar if snap.daily_bar and snap.daily_bar.timestamp.astimezone(ET).date() == session.day else None
        gap_pct = _pct(ask, prev_close)
        if cfg.MIN_GAP_PCT is not None and (gap_pct is None or gap_pct < cfg.MIN_GAP_PCT):
            reject(f"gap {gap_pct}% < {cfg.MIN_GAP_PCT}%")
            continue

        pm = premarket.get(t, {})
        context = {
            "candidate": c,
            "asset": asset,
            "quote_age_sec": quote_age,
            "open_gap_pct": _pct(today_bar.open, prev_close) if today_bar else None,
            "premarket": pm,
            "snapshot": {k: _dump(getattr(snap, k)) for k in ("latest_trade", "latest_quote", "minute_bar", "daily_bar", "previous_daily_bar")},
        }
        pid = store.open_position({
            "ticker": t, "source": c["source"], "status": "open", "qty": 1.0,
            "entry_day": session.day, "entry_time": now, "entry_price": ask, "entry_bid": bid,
            "entry_last": snap.latest_trade.price if snap.latest_trade else None,
            "spread_pct": round(spread_pct, 3), "prev_close": prev_close, "gap_pct": gap_pct,
            "premarket_volume": pm.get("volume"), "premarket_high": pm.get("high"),
            "news_score": c["score"], "news_confidence": c["confidence"],
            "catalysts": json.dumps(c.get("catalysts", [])), "event_type": c.get("event_type"),
            "entry_context": json.dumps(context, default=str), "peak_bid": bid, "peak_pnl_pct": _pct(bid, ask),
        })
        store.log_candidate(session.day, c, "entered")
        entered += 1
        logger.info(f"  BUY  ${t:<6} {c['source']:<5} #{pid} 1 @ {ask:.4f} (bid {bid:.4f}, spread {spread_pct:.2f}%, "
                    f"gap {gap_pct}%, score {c['score']:+.2f} conf {c['confidence']:.2f})")


def monitor_positions(store: SimStore, session, now: datetime):
    positions = store.open_positions()
    if not positions:
        return
    symbols = sorted({p["ticker"] for p in positions})
    snaps = _snapshots(symbols)
    try:
        bars = market.minute_bars(symbols, session.open, now)
    except Exception as e:
        logger.warning(f"Minute bars failed: {e}")
        bars = {}
    minutes_to_close = (session.close - now).total_seconds() / 60

    for p in positions:
        t = p["ticker"]
        snap = snaps.get(t)
        quote = snap.latest_quote if snap else None
        if not quote or not quote.bid_price:
            logger.warning(f"  no bid for ${t}, can't mark position #{p['id']}")
            continue
        bid, ask = quote.bid_price, quote.ask_price
        entry = p["entry_price"]
        pnl_pct = _pct(bid, entry)
        peak_bid = max(p["peak_bid"] or bid, bid)
        peak_pnl_pct = _pct(peak_bid, entry)

        try:
            recent = market.trades(t, now - timedelta(minutes=cfg.TRADE_LOOKBACK_MIN), now)
        except Exception as e:
            logger.warning(f"  trades failed for ${t}: {e}")
            recent = []
        flow = order_flow(recent, cfg.TRADE_LOOKBACK_MIN)
        climax = volume_climax(bars.get(t, []))
        days_held = market.trading_days_between(p["entry_day"], session.day)
        reason = exit_reason(pnl_pct, peak_pnl_pct, flow, climax, days_held, minutes_to_close)

        store.add_mark({
            "position_id": p["id"], "ts": now, "bid": bid, "ask": ask,
            "last": snap.latest_trade.price if snap.latest_trade else None,
            "pnl_pct": pnl_pct, "peak_pnl_pct": peak_pnl_pct, "drawdown_pct": round(peak_pnl_pct - pnl_pct, 3),
            **climax, **flow,
        })
        store.update_peak(p["id"], peak_bid, peak_pnl_pct)
        logger.info(f"  mark ${t:<6} #{p['id']} bid {bid:.4f} pnl {pnl_pct:+.2f}% peak {peak_pnl_pct:+.2f}% "
                    f"vol x{climax['volume_ratio']} buy {flow['buy_ratio']} tpm {flow['trades_per_min']} day {days_held}")

        if reason:
            pnl_usd = round((bid - entry) * p["qty"], 4)
            context = {"flow": flow, "climax": climax, "quote": _dump(quote), "minutes_to_close": minutes_to_close}
            store.close_position(p["id"], bid, reason, context, pnl_usd, pnl_pct, days_held)
            logger.info(f"  SELL ${t:<6} #{p['id']} @ {bid:.4f} {reason} pnl {pnl_pct:+.2f}% (${pnl_usd:+.4f}) after {days_held}d")


def summary(store: SimStore) -> dict:
    closed = store.query("SELECT * FROM positions WHERE status = 'closed' ORDER BY exit_time")
    opened = store.query("SELECT * FROM positions WHERE status = 'open' ORDER BY id")
    by_reason, by_source = {}, {}
    for p in closed:
        for key, bucket in ((p["exit_reason"], by_reason), (p["source"], by_source)):
            b = bucket.setdefault(key, {"trades": 0, "wins": 0, "pnl_pct_sum": 0.0})
            b["trades"] += 1
            b["wins"] += p["pnl_pct"] > 0
            b["pnl_pct_sum"] += p["pnl_pct"]
    for bucket in (by_reason, by_source):
        for b in bucket.values():
            b["avg_pnl_pct"] = round(b.pop("pnl_pct_sum") / b["trades"], 3)
    wins = [p for p in closed if p["pnl_pct"] > 0]
    return {
        "generated_at": datetime.now(ET).isoformat(),
        "closed_trades": len(closed),
        "win_rate_pct": round(len(wins) / len(closed) * 100, 1) if closed else None,
        "avg_pnl_pct": round(sum(p["pnl_pct"] for p in closed) / len(closed), 3) if closed else None,
        "total_pnl_usd": round(sum(p["pnl_usd"] for p in closed), 4),
        "by_exit_reason": by_reason,
        "by_source": by_source,
        "open_positions": [{k: p[k] for k in ("id", "ticker", "source", "entry_day", "entry_price", "peak_pnl_pct")} for p in opened],
        "closed": [{k: p[k] for k in ("id", "ticker", "source", "entry_day", "entry_price", "exit_price", "exit_reason",
                                      "pnl_pct", "pnl_usd", "days_held")} for p in closed],
    }


def write_report(store: SimStore) -> dict:
    report = summary(store)
    (cfg.DATA_DIR / "report_latest.json").write_text(json.dumps(report, indent=2, default=str))
    return report


def run(now: datetime):
    today = now.date()
    session = market.session_for(today)
    with SimStore() as store:
        if session is None:
            if not store.session_entry(today):
                store.set_session(today, "closed", True, "market closed")
                logger.info(f"Market closed on {today}; not trading")
            return
        if now < session.open - timedelta(minutes=5) or now > session.close + timedelta(minutes=5):
            return

        state = store.session_entry(today)
        if state is None:
            status = "trading" if session.normal_open else "late_open"
            store.set_session(today, status, False, f"open {session.open:%H:%M} close {session.close:%H:%M}")
            state = store.session_entry(today)
            logger.info(f"Session {today}: {status}, {session.open:%H:%M}-{session.close:%H:%M} ET")
        if now < session.open or now > session.close:
            return

        if not state["entries_done"]:
            if not session.normal_open and cfg.SKIP_LATE_OPEN:
                store.set_session(today, "late_open", True, "late open; entries skipped")
                logger.info("Late open today; skipping new entries")
            elif now > session.open + timedelta(minutes=cfg.ENTRY_WINDOW_MIN):
                store.set_session(today, state["status"], True, "missed entry window")
                logger.warning("Missed the entry window; no new entries today")
            elif now >= session.open + timedelta(minutes=cfg.ENTRY_DELAY_MIN):
                enter_positions(store, session, now)
                store.set_session(today, state["status"], True, "entries done")

        monitor_positions(store, session, now)
        write_report(store)


def preview(now: datetime):
    """Shows what would be bought at the next open, without writing anything."""
    session = market.session_for(now.date()) if now.time() < dtime(16) else None
    session = session or next(s for s in market.sessions() if s.open > now)
    prev = market.previous_session(session.day)
    with SimStore() as store:
        held = store.open_tickers()
    candidates, threshold = select_candidates(session, prev, now, held)
    print(f"Next session {session.day} {session.open:%H:%M}-{session.close:%H:%M} ET "
          f"(normal open: {session.normal_open}); news since {prev.close}; threshold {threshold:.3f}")
    for c in candidates:
        extra = f"{c['event_type']} on {c['event_date']}" if c["source"] == "event" else ",".join(c.get("catalysts", []))
        print(f"  ${c['ticker']:<6} {c['source']:<5} strength {c['strength']:+.3f} score {c['score']:+.2f} "
              f"conf {c['confidence']:.2f}  {extra}")


def parse_args():
    parser = argparse.ArgumentParser(description="Simulated news-driven trader (one pass).")
    parser.add_argument("--preview", action="store_true", help="List candidates for the next open without trading")
    parser.add_argument("--report", action="store_true", help="Print the performance summary")
    return parser.parse_args()


def main():
    args = parse_args()
    now = datetime.now(ET)
    if args.preview:
        preview(now)
        return 0
    if args.report:
        with SimStore() as store:
            print(json.dumps(write_report(store), indent=2, default=str))
        return 0

    lock = open(cfg.LOCK_PATH, "w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        logger.warning("Previous trader run still going; skipping")
        return 0
    run(now)
    return 0


if __name__ == "__main__":
    sys.exit(main())
