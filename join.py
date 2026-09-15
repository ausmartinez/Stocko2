import glob, json, pandas as pd

sw = pd.concat([pd.DataFrame(json.load(open(f)))
                for f in sorted(glob.glob('data/*/swing.json'))], ignore_index=True)

rows = []
for f in sorted(glob.glob('data/*/open.json')):
    s = json.load(open(f))
    for c in s['candidates']:
        rows.append({**c, 'session_date': s['session_date']})
sc = pd.DataFrame(rows)

df = sw.merge(sc, on=['session_date', 'symbol'], suffixes=('', '_scan'))
df = pd.concat([df.drop(columns=['return_pct']),
                pd.json_normalize(df['return_pct']).add_prefix('ret_')], axis=1)

d = df[df.flagged & ~df.faded & (df.exit_reason != 'open')].copy()
d['atr_pct'] = d.atr / d.entry_price * 100
d.to_csv('dat.csv')
