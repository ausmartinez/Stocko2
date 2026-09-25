from statistics import median

from sim import config as cfg


def order_flow(trades: list, minutes: float) -> dict:
    """Tick-test order flow: upticks count as buyer-initiated volume, downticks as seller-initiated."""
    if not trades or minutes <= 0:
        return {"trades_per_min": 0.0, "volume_per_min": 0.0, "buy_ratio": None}
    buy = sell = 0.0
    prev_price, side = None, 0
    for t in trades:
        if prev_price is not None:
            if t.price > prev_price:
                side = 1
            elif t.price < prev_price:
                side = -1
        if side > 0:
            buy += t.size
        elif side < 0:
            sell += t.size
        prev_price = t.price
    volume = sum(t.size for t in trades)
    return {
        "trades_per_min": round(len(trades) / minutes, 2),
        "volume_per_min": round(volume / minutes, 1),
        "buy_ratio": round(buy / (buy + sell), 3) if buy + sell else None,
    }


def volume_climax(bars: list) -> dict:
    """Last completed minute's volume relative to the session's typical minute."""
    if len(bars) < 3:
        return {"minute_volume": None, "median_minute_volume": None, "volume_ratio": None, "session_volume": None}
    last = bars[-1].volume
    typical = median(b.volume for b in bars[:-1]) or 1.0
    return {
        "minute_volume": last,
        "median_minute_volume": typical,
        "volume_ratio": round(last / typical, 2),
        "session_volume": sum(b.volume for b in bars),
    }


def exit_reason(pnl_pct: float, peak_pnl_pct: float, flow: dict, climax: dict,
                days_held: int, minutes_to_close: float):
    if pnl_pct <= -cfg.STOP_LOSS_PCT:
        return "stop_loss"
    if pnl_pct >= cfg.TAKE_PROFIT_PCT:
        return "take_profit"
    if peak_pnl_pct >= cfg.TRAIL_ACTIVATE_PCT and peak_pnl_pct - pnl_pct >= cfg.TRAIL_PCT:
        return "trailing_stop"
    # A burst of aggressive buying late in a run-up tends to mark the local top; sell into it
    if (pnl_pct >= cfg.CLIMAX_MIN_PNL_PCT
            and (climax["volume_ratio"] or 0) >= cfg.CLIMAX_VOLUME_MULT
            and (flow["buy_ratio"] or 0) >= cfg.CLIMAX_BUY_RATIO):
        return "buy_climax"
    if days_held >= cfg.MAX_HOLD_DAYS and minutes_to_close <= cfg.EOD_EXIT_MIN:
        return "max_hold"
    return None
