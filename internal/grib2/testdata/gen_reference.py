# Generates grib2 test fixtures and reference values with ecCodes (independent decoder).
# Usage: python -I gen_reference.py <in_dir> <out_dir>
import sys, os, json, math, calendar, datetime
import numpy as np
import eccodes as ec

src, out = sys.argv[1], sys.argv[2]
MISS = 9.999e20

def load(path):
    with open(path, 'rb') as f:
        return ec.codes_new_from_file(f, ec.CODES_PRODUCT_GRIB)

def save(h, name):
    with open(os.path.join(out, name), 'wb') as f:
        ec.codes_write(h, f)

def g(h, k):
    return ec.codes_get(h, k)

def reference(name):
    h = load(os.path.join(out, name))
    ec.codes_set(h, 'missingValue', MISS)
    v = np.array(ec.codes_get_values(h), dtype=np.float64)
    ni, nj = g(h, 'Ni'), g(h, 'Nj')
    n = ni * nj
    assert len(v) == n
    miss = v == MISS
    idx = {0, 1, ni - 1, ni, n - ni, n - 1, n // 2, (nj // 2) * ni + ni // 2}
    for i in range(1, 17):
        idx.add(i * n // 17 + i * 7)
    if miss.any():
        m = np.flatnonzero(miss); p = np.flatnonzero(~miss)
        for k in (0, len(m) // 2, len(m) - 1): idx.add(int(m[k]))
        for k in (0, len(p) // 2, len(p) - 1): idx.add(int(p[k]))
    idx = sorted(i for i in idx if 0 <= i < n)
    good = v[~miss]
    sf = g(h, 'scaleFactorOfFirstFixedSurface'); sv = g(h, 'scaledValueOfFirstFixedSurface')
    level = float(sv) * 10.0 ** (-sf) if sf != ec.CODES_MISSING_LONG and sv != ec.CODES_MISSING_LONG else 0.0
    dd, dt = g(h, 'dataDate'), g(h, 'dataTime')
    ref = calendar.timegm(datetime.datetime(dd // 10000, dd // 100 % 100, dd % 100, dt // 100, dt % 100).timetuple())
    unit = g(h, 'indicatorOfUnitOfTimeRange'); ft = g(h, 'forecastTime')
    hours = {0: ft / 60, 1: ft, 2: ft * 24}[unit]
    jpos = g(h, 'jScansPositively')
    r = {
        'file': name,
        'drt': g(h, 'dataRepresentationTemplateNumber'),
        'bitmap': bool(ec.codes_get_long(h, 'bitmapPresent')),
        'discipline': g(h, 'discipline'),
        'category': g(h, 'parameterCategory'), 'number': g(h, 'parameterNumber'),
        'surfaceType': ec.codes_get_long(h, 'typeOfFirstFixedSurface'), 'surfaceValue': level,
        'refTime': ref, 'forecastHour': int(hours),
        'ni': ni, 'nj': nj,
        'la1': g(h, 'latitudeOfFirstGridPointInDegrees'), 'lo1': g(h, 'longitudeOfFirstGridPointInDegrees'),
        'dlat': g(h, 'jDirectionIncrementInDegrees') * (1 if jpos else -1),
        'dlon': g(h, 'iDirectionIncrementInDegrees'),
        'missing': int(miss.sum()),
        'min': float(good.min()), 'max': float(good.max()), 'mean': float(good.mean()),
        'samples': [[int(i), None if miss[i] else float(v[i])] for i in idx],
    }
    ec.codes_release(h)
    return r

fixtures = {'tmp850_drt53.grib2': 'm462.grb2', 'rh2m_drt53.grib2': 'm584.grb2',
            'hgtsfc_drt53.grib2': 'm562.grb2', 'weasd_drt53_bitmap.grib2': 'm577.grb2'}
for dst, s in fixtures.items():
    with open(os.path.join(src, s), 'rb') as f, open(os.path.join(out, dst), 'wb') as o:
        o.write(f.read())

# Re-encodings of TMP 850 mb by ecCodes, for the templates GFS does not use.
def derive(name, **keys):
    h = load(os.path.join(src, 'm462.grb2'))
    vals = ec.codes_get_values(h)
    for k, val in keys.items():
        if k not in ('flip', 'holes'):
            ec.codes_set(h, k, val)
    if keys.get('holes'):
        # every 7th point missing, encoded by missing value management (no bitmap)
        ec.codes_set(h, 'missingValue', MISS)
        ec.codes_set(h, 'bitmapPresent', 0)
        ec.codes_set(h, 'missingValueManagementUsed', 1)
        vals[::7] = MISS
    if keys.get('flip'):
        ni, nj = g(h, 'Ni'), g(h, 'Nj')
        la1, la2 = g(h, 'latitudeOfFirstGridPoint'), g(h, 'latitudeOfLastGridPoint')
        ec.codes_set(h, 'jScansPositively', 1)
        ec.codes_set(h, 'latitudeOfFirstGridPoint', la2)
        ec.codes_set(h, 'latitudeOfLastGridPoint', la1)
        vals = vals.reshape(nj, ni)[::-1].reshape(-1)
    ec.codes_set_values(h, vals)
    save(h, name)
    ec.codes_release(h)

derive('tmp850_drt50.grib2', packingType='grid_simple')
derive('tmp850_drt52.grib2', packingType='grid_complex')
derive('tmp850_drt53_order1.grib2', packingType='grid_complex_spatial_differencing', orderOfSpatialDifferencing=1)
derive('tmp850_drt53_jpos.grib2', flip=True)
derive('tmp850_drt52_mvm.grib2', packingType='grid_complex', holes=True)
derive('tmp850_drt53_mvm.grib2', packingType='grid_complex_spatial_differencing', holes=True)

refs = [reference(n) for n in sorted(os.listdir(out)) if n.endswith('.grib2')]
with open(os.path.join(out, 'reference.json'), 'w') as f:
    f.write('{"generator": %s, "fields": [\n' % json.dumps('ecCodes ' + ec.codes_get_api_version()))
    f.write(',\n'.join(json.dumps(r) for r in refs))
    f.write('\n]}\n')
for r in refs:
    print(r['file'], 'drt', r['drt'], 'miss', r['missing'], r['min'], r['max'])
