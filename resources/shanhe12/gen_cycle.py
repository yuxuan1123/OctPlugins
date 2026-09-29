# -*- coding: utf-8 -*-
"""日月山河 · 十二时辰序列：12 张 10:1 国风山水长卷"""
import math, random

W, H = 3000, 300
HORIZON = 222


# ---------------------------------------------------------------- 基础工具
def hex2rgb(h):
    h = h.lstrip('#')
    return tuple(int(h[i:i + 2], 16) for i in (0, 2, 4))


def rgb2hex(c):
    return '#%02X%02X%02X' % tuple(max(0, min(255, int(round(v)))) for v in c)


def mix(a, b, t):
    ca, cb = hex2rgb(a), hex2rgb(b)
    return rgb2hex([ca[i] + (cb[i] - ca[i]) * t for i in range(3)])


def smoothstep(a, b, x):
    t = max(0.0, min(1.0, (x - a) / (b - a)))
    return t * t * (3 - 2 * t)


def catmull(points):
    x0, y0 = points[0]
    d = [f"M {x0:.0f} {y0:.0f}"]
    px, py = x0, y0
    n = len(points)
    for i in range(n - 1):
        p0 = points[i - 1] if i - 1 >= 0 else points[0]
        p1, p2 = points[i], points[i + 1]
        p3 = points[i + 2] if i + 2 < n else points[-1]
        c1x = p1[0] + (p2[0] - p0[0]) / 6.0
        c1y = p1[1] + (p2[1] - p0[1]) / 6.0
        c2x = p2[0] - (p3[0] - p1[0]) / 6.0
        c2y = p2[1] - (p3[1] - p1[1]) / 6.0
        d.append(f"c {c1x-px:.0f} {c1y-py:.0f} {c2x-px:.0f} {c2y-py:.0f} {p2[0]-px:.0f} {p2[1]-py:.0f}")
        px, py = p2
    return "".join(d)


def ridge(seed, base_y, amp, npeaks, valley_x=None, samples=20, top_lim=70.0):
    rnd = random.Random(seed)
    peaks = [( (i + rnd.uniform(0.15, 0.85)) / npeaks * 1.06 - 0.03,
               rnd.uniform(0.45, 1.0), rnd.uniform(0.045, 0.145)) for i in range(npeaks)]
    pts = []
    for i in range(samples):
        t = i / (samples - 1)
        x = t * W
        y = base_y
        for (c, h, w) in peaks:
            y -= amp * h * math.exp(-((t - c) / w) ** 2)
        y -= amp * 0.05 * math.sin(t * 9.3 + seed * 1.7) + amp * 0.03 * math.sin(t * 19.1 + seed * 0.9)
        f = (0.18 + 0.82 * smoothstep(0.0, 340.0, x)) * (1 - 0.30 * smoothstep(2720.0, W, x))
        y = base_y - (base_y - y) * f
        if valley_x is not None:
            g = math.exp(-((x - valley_x) / 250.0) ** 2)
            y = base_y - (base_y - y) * (1 - 0.55 * g)
        if y < top_lim:
            d = top_lim - y
            y = top_lim - 20 * (1 - math.exp(-d / 20.0))
        pts.append((x, y))
    return pts


def fill_path(pts, bottom):
    return catmull(pts) + "L 3000 %.0f L 0 %.0f Z" % (bottom, bottom)


def pine(x, base, h, color, seed=1, op=1.0):
    rnd = random.Random(seed)
    bend = rnd.uniform(-7, 7)
    out = [f'<path d="M {x:.0f} {base:.0f} C {x+bend*0.3:.0f} {base-h*0.45:.0f} '
           f'{x-bend*0.2:.0f} {base-h*0.75:.0f} {x+bend:.0f} {base-h*0.95:.0f}" '
           f'stroke="{color}" stroke-width="{max(1.8, h*0.06):.1f}" fill="none" '
           f'stroke-linecap="round" opacity="{op:.2f}"/>']
    for (frac, wf) in [(0.52, 1.0), (0.75, 0.75), (0.96, 0.42)]:
        cy = base - h * frac
        w = h * 0.34 * wf
        dh = h * 0.17 * wf
        out.append(f'<path d="M {x-w:.0f} {cy:.0f} Q {x-w*0.5:.0f} {cy-dh*1.7:.0f} {x:.0f} {cy-dh*2.0:.0f} '
                   f'Q {x+w*0.5:.0f} {cy-dh*1.7:.0f} {x+w:.0f} {cy:.0f} Z" fill="{color}" opacity="{op:.2f}"/>')
    return "\n".join(out)


def boat(x, y, color):
    return (f'<g transform="translate({x},{y})" opacity="0.9">'
            f'<path d="M -46,0 C -28,11 28,11 46,0 C 26,6 -26,6 -46,0 Z" fill="{color}"/>'
            f'<path d="M 8,-2 L 8,-15" stroke="{color}" stroke-width="2.4" stroke-linecap="round"/>'
            f'<circle cx="8" cy="-19" r="3.4" fill="{color}"/>'
            f'<path d="M 8,-12 L 30,-24" stroke="{color}" stroke-width="2" stroke-linecap="round" opacity="0.85"/>'
            f'</g>')


def ripples(color, seed, n, op):
    rnd = random.Random(seed)
    out = []
    for _ in range(n):
        y = HORIZON + 7 + rnd.uniform(0, 70)
        x0 = rnd.uniform(-40, 2900)
        w = rnd.uniform(70, 340)
        amp = rnd.uniform(0.6, 2.4)
        o = op * rnd.uniform(0.22, 1.0)
        out.append(f'<path d="M {x0:.0f} {y:.0f} q {w*0.25:.0f} {-amp:.0f} {w*0.5:.0f} 0 '
                   f'q {w*0.25:.0f} {amp:.0f} {w*0.5:.0f} 0" fill="none" stroke="{color}" '
                   f'stroke-width="{rnd.uniform(0.9,1.8):.0f}" opacity="{o:.2f}" stroke-linecap="round"/>')
    return "\n".join(out)


def bird(x, y, s, color, op):
    return (f'<path d="M {x:.0f} {y:.0f} q {s*0.5:.1f} {-s*0.55:.1f} {s:.0f} 0 '
            f'q {s*0.5:.1f} {-s*0.55:.1f} {s:.0f} 0" fill="none" stroke="{color}" '
            f'stroke-width="2" stroke-linecap="round" opacity="{op:.2f}"/>')


def ruyi_cloud(x, y, s, color, op, seed=1, sw=2.0):
    rnd = random.Random(seed)
    tail = rnd.uniform(90, 190)
    d1 = "M 0,0 c -9,-11 4,-24 17,-17 c 11,6 8,20 -2,22 c -9,1.6 -13,-3.4 -12,-8.5"
    d2 = f"M 14,-6 c {tail*0.35:.0f},-6 {tail*0.68:.0f},-3 {tail:.0f},-9"
    d3 = f"M 10,8 c {tail*0.3:.0f},4 {tail*0.62:.0f},1 {tail*0.9:.0f},6"
    return (f'<g transform="translate({x},{y}) scale({s:.2f})" fill="none" stroke="{color}" '
            f'stroke-width="{sw}" stroke-linecap="round" opacity="{op:.2f}">'
            f'<path d="{d1}"/><path d="{d2}" opacity="0.75"/><path d="{d3}" opacity="0.5"/></g>')


def soft_cloud(cx, cy, rx, ry, op):
    return f'<ellipse cx="{cx}" cy="{cy}" rx="{rx}" ry="{ry}" fill="url(#cloudGrad)" opacity="{op:.2f}"/>'


def seal(x, y, kind, bg, fg):
    if kind == "sun":
        strokes = ('<rect x="13" y="10" width="20" height="26" rx="2"/>'
                   '<path d="M 13,23 L 33,23"/>')
    else:
        strokes = ('<path d="M 15,11 C 13.5,18 12.5,25 10,31"/>'
                   '<path d="M 15,11 L 30,11 L 30,24 C 30,29.5 26,32.5 22,32.5"/>'
                   '<path d="M 15.5,18.2 L 30,18.2"/><path d="M 14.5,25.4 L 30,25.4"/>')
    return (f'<g transform="translate({x},{y}) scale(0.957)">'
            f'<rect width="46" height="46" rx="5" fill="{bg}" opacity="0.9"/>'
            f'<rect x="2.5" y="2.5" width="41" height="41" rx="3.5" fill="none" stroke="{fg}" '
            f'stroke-width="1.2" opacity="0.55"/>'
            f'<g fill="none" stroke="{fg}" stroke-width="2.6" stroke-linecap="round" '
            f'stroke-linejoin="round">{strokes}</g></g>')


# ---------------------------------------------------------------- 十二时辰配置
F = []
def fr(**kw):
    F.append(kw)

# 1 破晓：残月西沉，东方既白
fr(no='01', name='破晓', hour='卯时', orb='moon', ox=560, oy=148, or_=34, gr=170,
   sky=['#1A2340', '#2C3658', '#565A72', '#94808A', '#CFA089'],
   core=['#F8F4E4', '#EAE7D6', '#CFD2BE'], glow=['#E8EEF6', '#B9C6DA', '#7E8CA0', '#5A6478'],
   ring='#C9D4E2', ringop='0.28', far='#5A6480', near='#161E30', edge='#B9C6DA',
   water=['#6B6A72', '#454A5A', '#262B38'], mist='#D8C4B4', cloud='#C4B2AC', ck=0.55,
   stars=(10, 0.35), beam=('#E8C9A8', 0.35, 120), bank='#121A2A', pine='#0E1522',
   ripd='#2A3242', ripl='#C9B8AE', frame='#6E7C92', frameop='0.35',
   sealbg='#8E3A30', sealfg='#F2E8DA', birds=1, birdc='#2A3040', birdop=0.7)

# 2 日出：日轮初升于山坳
fr(no='02', name='日出', hour='辰时', orb='sun', ox=470, oy=146, or_=40, gr=230,
   sky=['#7C86A8', '#B99A9E', '#E8A878', '#F5C98E', '#FBE0B4'],
   core=['#FFF6DC', '#F9B45E', '#E86A38'], glow=['#FDE9B8', '#F7A85E', '#E8703C', '#C24A2C'],
   ring='#F9D79C', ringop='0.50', far='#847E92', near='#241F2C', edge='#F9B45E',
   water=['#F2C79A', '#D9A183', '#A07C70'], mist='#FBE4C4', cloud='#FDE8CE', ck=1.0,
   stars=(0, 0), beam=('#F6C88E', 0.85, 170), bank='#1E2630', pine='#18202A',
   ripd='#7A5A48', ripl='#FFF0D8', frame='#B08D57', frameop='0.45',
   sealbg='#B23A2C', sealfg='#FBF3E4', birds=3, birdc='#4A4038', birdop=0.7)

# 3 朝：天光清朗
fr(no='03', name='朝晖', hour='巳时', orb='sun', ox=720, oy=116, or_=40, gr=215,
   sky=['#9CBCD6', '#C8D6DA', '#EFE2CC', '#F7EBD6', '#FAF0E2'],
   core=['#FFF8E4', '#FBC96E', '#F09A46'], glow=['#FCEFC4', '#F8C77E', '#EFA85E', '#E08A50'],
   ring='#F9DFA8', ringop='0.45', far='#9CA9A2', near='#2E4A44', edge='#FBC96E',
   water=['#F6E2C4', '#E4C9A8', '#C4A88C'], mist='#FFF4E2', cloud='#FFFAF0', ck=1.0,
   stars=(0, 0), beam=('#F8D29A', 0.70, 150), bank='#223B39', pine='#1B3130',
   ripd='#8C6A45', ripl='#FFFDF6', frame='#B08D57', frameop='0.45',
   sealbg='#B23A2C', sealfg='#FBF3E4', birds=3, birdc='#4A4038', birdop=0.7)

# 4 近午
fr(no='04', name='近午', hour='午初', orb='sun', ox=1010, oy=74, or_=40, gr=210,
   sky=['#8FB4D4', '#B8CEDD', '#E9E4D2', '#F5EEDC', '#FAF4E8'],
   core=['#FFFCF0', '#FDDC90', '#F5B660'], glow=['#FFF3CE', '#FAD992', '#F0B775', '#E8A466'],
   ring='#FCE9B8', ringop='0.35', far='#A2B2A8', near='#2E4C46', edge='#FDDC90',
   water=['#F2E4CA', '#DCC7A8', '#BEAA90'], mist='#FFFAEE', cloud='#FFFDF6', ck=1.0,
   stars=(0, 0), beam=('#F6DCA8', 0.60, 130), bank='#1F3836', pine='#18302E',
   ripd='#8A6C4C', ripl='#FFFEF8', frame='#B08D57', frameop='0.45',
   sealbg='#B23A2C', sealfg='#FBF3E4', birds=3, birdc='#4A4038', birdop=0.7)

# 5 正午：日高悬，光最盛
fr(no='05', name='正午', hour='午正', orb='sun', ox=1330, oy=44, or_=38, gr=205,
   sky=['#7EA9CE', '#A8C6DD', '#E4E7DA', '#F6F3E6', '#FBF8EE'],
   core=['#FFFFFA', '#FFF2C8', '#FBD98E'], glow=['#FFF8DC', '#FCEDB4', '#F2D69A', '#EAC888'],
   ring='#FFF6D8', ringop='0.30', far='#AAB8AC', near='#315046', edge='#FFF2C8',
   water=['#EFE6CE', '#D6C4A6', '#B8A88E'], mist='#FFFDF4', cloud='#FFFFFF', ck=1.0,
   stars=(0, 0), beam=('#FAE8BC', 0.50, 110), bank='#1E3735', pine='#17302D',
   ripd='#8A7052', ripl='#FFFFFF', frame='#B08D57', frameop='0.45',
   sealbg='#B23A2C', sealfg='#FBF3E4', birds=3, birdc='#4A4038', birdop=0.7)

# 6 午后
fr(no='06', name='午后', hour='未时', orb='sun', ox=1780, oy=62, or_=40, gr=210,
   sky=['#86AECF', '#B4CBDC', '#EBE2CE', '#F8EEDC', '#FBEFDC'],
   core=['#FFFCF0', '#FCE0A0', '#F4B86A'], glow=['#FFF3CE', '#FADE9E', '#F2BC7C', '#EAAA70'],
   ring='#FCEFC4', ringop='0.32', far='#A6B4A8', near='#2F4C44', edge='#FCE0A0',
   water=['#F1E4C8', '#D9C6A4', '#BAA98E'], mist='#FFFBF0', cloud='#FFFDF8', ck=1.0,
   stars=(0, 0), beam=('#F8E0AC', 0.55, 120), bank='#1F3836', pine='#18302E',
   ripd='#8A6A48', ripl='#FFFEF6', frame='#B08D57', frameop='0.45',
   sealbg='#B23A2C', sealfg='#FBF3E4', birds=3, birdc='#4A4038', birdop=0.7)

# 7 黄昏：金乌西斜
fr(no='07', name='黄昏', hour='申时', orb='sun', ox=2200, oy=108, or_=42, gr=220,
   sky=['#7E9CC0', '#C2B0AE', '#EFB888', '#F8D3A0', '#FBE2B8'],
   core=['#FFF6DC', '#FBBE72', '#EE8A44'], glow=['#FDEAC0', '#F9BE7E', '#EE9A56', '#D87040'],
   ring='#F9DFA8', ringop='0.45', far='#9AA08E', near='#2C3C34', edge='#FBBE72',
   water=['#F2D2A4', '#DCAE88', '#B89076'], mist='#FBE8C8', cloud='#FFFAF0', ck=1.0,
   stars=(0, 0), beam=('#F8CE96', 0.70, 145), bank='#223029', pine='#1B2A24',
   ripd='#7E5A42', ripl='#FFF3DE', frame='#B08D57', frameop='0.45',
   sealbg='#B23A2C', sealfg='#FBF3E4', birds=3, birdc='#4A4038', birdop=0.7)

# 8 日落：白日依山尽
fr(no='08', name='日落', hour='酉时', orb='sun', ox=2660, oy=158, or_=44, gr=240,
   sky=['#5C5C86', '#8E6E8C', '#D88A6E', '#F0A878', '#F8C89A'],
   core=['#FFF2CE', '#F89A50', '#D84A28'], glow=['#FDD9A4', '#F09A58', '#D85E34', '#A83A24'],
   ring='#F8C88A', ringop='0.50', far='#8E7A84', near='#221E2E', edge='#F89A50',
   water=['#F0B890', '#C88870', '#8E5E56'], mist='#FADDB8', cloud='#FFEEDA', ck=1.0,
   stars=(0, 0), beam=('#F4A874', 0.85, 190), bank='#1A1622', pine='#151220',
   ripd='#6E4038', ripl='#FFE8C4', frame='#A87A5C', frameop='0.45',
   sealbg='#B23A2C', sealfg='#FBF3E4', birds=2, birdc='#4A3028', birdop=0.75)

# 9 初夜：月出于东山之上
fr(no='09', name='初夜', hour='戌时', orb='moon', ox=2720, oy=155, or_=36, gr=185,
   sky=['#101A32', '#1C2C4A', '#334E68', '#5E7286', '#8E8A88'],
   core=['#FFFBE8', '#F6EFD4', '#DCDCC4'], glow=['#EAF2FA', '#BED4E8', '#8AA4BE', '#5B7A94'],
   ring='#DCEBF7', ringop='0.30', far='#3A4C66', near='#101A2A', edge='#BED4E8',
   water=['#3A5266', '#24384C', '#131E2E'], mist='#A8B8C8', cloud='#9FB6CC', ck=0.8,
   stars=(12, 0.50), beam=('#CFE0F0', 0.50, 140), bank='#0A1120', pine='#070D18',
   ripd='#2A3A4C', ripl='#DCE8F4', frame='#7E93AC', frameop='0.35',
   sealbg='#A8382C', sealfg='#F7EFE0', birds=1, birdc='#0C1826', birdop=0.8)

# 10 夜
fr(no='10', name='夜色', hour='亥时', orb='moon', ox=2320, oy=88, or_=37, gr=190,
   sky=['#080E1E', '#101C34', '#1A2E4A', '#23405E', '#2C5474'],
   core=['#FFFDF2', '#F2EEDC', '#D9DCCB'], glow=['#E8F3FA', '#BBD8EC', '#7FA9C8', '#5B87A8'],
   ring='#DCEBF7', ringop='0.30', far='#2E4763', near='#101F34', edge='#BBD8EC',
   water=['#2A4A66', '#1A324C', '#0C1A2A'], mist='#A8C8E0', cloud='#A9C7E3', ck=0.9,
   stars=(22, 0.70), beam=('#CFE6F5', 0.60, 130), bank='#060D18', pine='#050B14',
   ripd='#1E2E42', ripl='#EAF4FC', frame='#8FA9C4', frameop='0.35',
   sealbg='#A8382C', sealfg='#F7EFE0', birds=0, birdc='#0C1826', birdop=0.8)

# 11 夜半：月华如水
fr(no='11', name='夜半', hour='子时', orb='moon', ox=1600, oy=54, or_=38, gr=195,
   sky=['#050A16', '#0A1428', '#12243E', '#183150', '#1E4258'],
   core=['#FFFFFB', '#F8F6E8', '#E0E2D2'], glow=['#EDF6FD', '#C6DDF0', '#8EB2CE', '#6290B0'],
   ring='#E4F0FA', ringop='0.30', far='#2A4360', near='#0D1B2E', edge='#C6DDF0',
   water=['#22405C', '#152B44', '#091625'], mist='#A8C8E0', cloud='#A9C7E3', ck=0.9,
   stars=(34, 0.85), beam=('#D8EAF8', 0.65, 120), bank='#050B14', pine='#04080F',
   ripd='#18283A', ripl='#F2F8FD', frame='#8FA9C4', frameop='0.35',
   sealbg='#A8382C', sealfg='#F7EFE0', birds=0, birdc='#0C1826', birdop=0.8)

# 12 将晓：残月将坠，星河渐隐
fr(no='12', name='将晓', hour='寅时', orb='moon', ox=860, oy=150, or_=35, gr=175,
   sky=['#0A1020', '#141F36', '#22334C', '#3A4A60', '#5E6470'],
   core=['#F8F4E4', '#EAE6D4', '#D2D2C0'], glow=['#E6EEF8', '#B8C8DC', '#8398B0', '#5E7288'],
   ring='#D4E0EE', ringop='0.28', far='#3C4C66', near='#121C2C', edge='#B8C8DC',
   water=['#344A60', '#22344A', '#111C2A'], mist='#B0C0CE', cloud='#A6B8C8', ck=0.85,
   stars=(18, 0.50), beam=('#C8D8E8', 0.45, 135), bank='#080F1A', pine='#060C14',
   ripd='#22303E', ripl='#DCE6F0', frame='#7A8CA4', frameop='0.35',
   sealbg='#A8382C', sealfg='#F7EFE0', birds=1, birdc='#0C1826', birdop=0.8)


# ---------------------------------------------------------------- 生成
MOUNTAINS = [(11, 200, 132, 6, 18), (22, 206, 104, 7, 20), (33, 212, 74, 8, 22), (44, 222, 46, 9, 24)]
BANK = (77, 223, 15, 5)
HIGH_CLOUD_D = [(520, 78, 190, 16, 0.42), (1180, 62, 150, 12, 0.34), (2380, 66, 170, 13, 0.30)]
HIGH_CLOUD_N = [(430, 74, 190, 14, 0.30), (1500, 96, 220, 15, 0.30), (2760, 112, 150, 12, 0.22)]
RUYI_D = [(1080, 58, 1.05, 0.35), (2260, 70, 0.95, 0.30)]
RUYI_N = [(700, 52, 1.00, 0.30), (2580, 66, 0.90, 0.26)]
MIST_D = [(900, 176, 260, 13, 0.50), (1520, 192, 300, 12, 0.45), (2650, 200, 220, 11, 0.40)]
MIST_N = [(760, 178, 250, 12, 0.30), (1320, 194, 300, 11, 0.26), (2680, 202, 200, 10, 0.22)]
BIRD_POS = [(1420, 96, 13), (1470, 118, 10), (1900, 130, 9), (1232, 126, 9), (1180, 104, 12)]


def build(cfg):
    night = cfg['orb'] == 'moon'
    ox, oy, orr = cfg['ox'], cfg['oy'], cfg['or_']
    s = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}">']

    # defs
    d = ['<defs>',
         '<filter id="reflBlur" x="-10%" y="-10%" width="120%" height="120%">'
         '<feGaussianBlur stdDeviation="3.2"/></filter>',
         f'<clipPath id="waterClip"><rect x="0" y="{HORIZON}" width="{W}" height="{H-HORIZON}"/></clipPath>',
         f'<mask id="reflFade" maskUnits="userSpaceOnUse" x="0" y="{HORIZON}" width="{W}" height="{H-HORIZON}">'
         f'<rect x="0" y="{HORIZON}" width="{W}" height="{H-HORIZON}" fill="url(#reflFadeGrad)"/></mask>',
         f'<linearGradient id="reflFadeGrad" gradientUnits="userSpaceOnUse" x1="0" y1="{HORIZON}" x2="0" y2="{H}">'
         '<stop offset="0" stop-color="#fff" stop-opacity="0.95"/>'
         '<stop offset="0.45" stop-color="#fff" stop-opacity="0.45"/>'
         '<stop offset="1" stop-color="#fff" stop-opacity="0"/></linearGradient>']
    stops = "".join(f'<stop offset="{o}" stop-color="{c}"/>' for o, c in
                    zip(['0', '0.22', '0.48', '0.74', '1'], cfg['sky']))
    d.append(f'<linearGradient id="skyG" x1="0" y1="0" x2="0" y2="1">{stops}</linearGradient>')
    stops = "".join(f'<stop offset="{o}" stop-color="{c}" stop-opacity="{a}"/>' for o, c, a in
                    zip(['0', '0.3', '0.62', '1'], cfg['glow'], ['0.9', '0.34', '0.12', '0']))
    d.append(f'<radialGradient id="orbGlow">{stops}</radialGradient>')
    stops = "".join(f'<stop offset="{o}" stop-color="{c}"/>' for o, c in
                    zip(['0', '0.45', '1'], cfg['core']))
    d.append(f'<radialGradient id="orbCore" cx="0.38" cy="0.34" r="0.72">{stops}</radialGradient>')
    d.append(f'<linearGradient id="mistG" x1="0" y1="0" x2="0" y2="1">'
             + "".join(f'<stop offset="{o}" stop-color="{cfg["mist"]}" stop-opacity="{a}"/>'
                       for o, a in zip(['0', '0.5', '1'], ['0', '0.85', '0'])) + '</linearGradient>')
    d.append(f'<linearGradient id="waterG" x1="0" y1="0" x2="0" y2="1">'
             + "".join(f'<stop offset="{o}" stop-color="{c}"/>' for o, c in
                       zip(['0', '0.45', '1'], cfg['water'])) + '</linearGradient>')
    d.append(f'<radialGradient id="cloudG">'
             f'<stop offset="0" stop-color="{cfg["cloud"]}" stop-opacity="0.95"/>'
             f'<stop offset="0.45" stop-color="{cfg["cloud"]}" stop-opacity="0.45"/>'
             f'<stop offset="1" stop-color="{cfg["cloud"]}" stop-opacity="0"/></radialGradient>')
    bc = cfg['beam'][0]
    d.append(f'<linearGradient id="beamG" x1="0" y1="0" x2="1" y2="0">'
             f'<stop offset="0" stop-color="{bc}" stop-opacity="0"/>'
             f'<stop offset="0.5" stop-color="{bc}" stop-opacity="0.55"/>'
             f'<stop offset="1" stop-color="{bc}" stop-opacity="0"/></linearGradient>')
    d.append('</defs>')
    s.append("\n".join(d))

    s.append(f'<rect width="{W}" height="{H}" fill="url(#skyG)"/>')

    # 星
    nstar, smax = cfg['stars']
    if nstar:
        rnd = random.Random(101)
        pts = []
        for _ in range(nstar * 3):
            sx, sy = rnd.uniform(0, W), rnd.uniform(6, 168)
            if abs(sx - ox) < 190 and abs(sy - oy) < 150:
                continue
            pts.append((sx, sy, rnd.uniform(0.7, 1.8), rnd.uniform(0.3, 1.0)))
            if len(pts) >= nstar:
                break
        for (sx, sy, r, o) in pts:
            s.append(f'<circle cx="{sx:.0f}" cy="{sy:.0f}" r="{r:.1f}" fill="#EAF2FB" '
                     f'opacity="{o*smax:.2f}"/>')

    # 云
    for (cx, cy, rx, ry, op) in (HIGH_CLOUD_N if night else HIGH_CLOUD_D):
        s.append(soft_cloud(cx, cy, rx, ry, op * cfg['ck']))
    for (cx, cy, sc, op) in (RUYI_N if night else RUYI_D):
        s.append(ruyi_cloud(cx, cy, sc, cfg['edge'] if night else cfg['ring'], op, 5))

    # 山（远两层）
    lc = [mix(cfg['far'], cfg['near'], i / 3.0) for i in range(4)]
    ops = [0.62, 0.75, 0.90, 1.0]
    pts_all = []
    for i, (seed, base, amp, np_, smp) in enumerate(MOUNTAINS):
        pts = ridge(seed, base, amp, np_, valley_x=ox, samples=smp)
        pts_all.append(pts)
    for i in (0, 1):
        seed, base, amp, np_, smp = MOUNTAINS[i]
        mo = [0.30, 0.28, 0.22, 0.20][i] * (0.85 if night else 1.0)
        s.append(f'<rect x="0" y="{base-8}" width="{W}" height="26" fill="url(#mistG)" opacity="{mo:.2f}"/>')
        s.append(f'<path id="m{i}" d="{fill_path(pts_all[i], HORIZON+2)}" fill="{lc[i]}" opacity="{ops[i]:.2f}"/>')

    # 天体（出于远山之后、近山之前）
    s.append(f'<circle cx="{ox}" cy="{oy}" r="{cfg["gr"]}" fill="url(#orbGlow)"/>')
    s.append(f'<circle cx="{ox}" cy="{oy}" r="{orr+16}" fill="none" stroke="{cfg["ring"]}" '
             f'stroke-width="1.4" opacity="{cfg["ringop"]}"/>')
    s.append(f'<circle cx="{ox}" cy="{oy}" r="{orr}" fill="url(#orbCore)"/>')
    if night:
        s.append(f'<g opacity="0.16" fill="#8C93A0">'
                 f'<circle cx="{ox-11}" cy="{oy-9}" r="8"/><circle cx="{ox+10}" cy="{oy+5}" r="5.5"/>'
                 f'<circle cx="{ox-4}" cy="{oy+15}" r="4"/><circle cx="{ox+13}" cy="{oy-16}" r="6.5"/></g>')

    # 山（近两层）
    for i in (2, 3):
        seed, base, amp, np_, smp = MOUNTAINS[i]
        mo = [0.30, 0.28, 0.22, 0.20][i] * (0.85 if night else 1.0)
        s.append(f'<rect x="0" y="{base-8}" width="{W}" height="26" fill="url(#mistG)" opacity="{mo:.2f}"/>')
        s.append(f'<path id="m{i}" d="{fill_path(pts_all[i], HORIZON+2)}" fill="{lc[i]}" opacity="{ops[i]:.2f}"/>')
        s.append(f'<path d="{catmull(pts_all[i])}" fill="none" stroke="{cfg["edge"]}" '
                 f'stroke-width="1.4" opacity="{0.32 if night else 0.28:.2f}"/>')
    for (cx, cy, rx, ry, op) in (MIST_N if night else MIST_D):
        s.append(soft_cloud(cx, cy, rx, ry, op * cfg['ck']))

    # 近岸与松
    bank = ridge(BANK[0], BANK[1], BANK[2], BANK[3], valley_x=ox, samples=16)
    s.append(f'<path id="bk" d="{fill_path(bank, HORIZON+2)}" fill="{cfg["bank"]}"/>')
    s.append(pine(486, 222, 74, cfg['pine'], 9))
    s.append(pine(536, 224, 52, cfg['pine'], 10, 0.92))
    s.append(pine(2832, 224, 58, cfg['pine'], 11))

    # 水
    s.append(f'<rect x="0" y="{HORIZON}" width="{W}" height="{H-HORIZON}" fill="url(#waterG)"/>')
    rop = 0.50 if night else 0.42
    s.append(f'<g clip-path="url(#waterClip)"><g mask="url(#reflFade)">'
             f'<g transform="translate(0,{2*HORIZON}) scale(1,-1)" opacity="{rop}" filter="url(#reflBlur)">'
             f'<use href="#m3"/><use href="#m2"/><use href="#bk"/></g></g></g>')
    bw = cfg['beam'][2]
    s.append(f'<g clip-path="url(#waterClip)"><rect x="{ox-bw//2}" y="{HORIZON}" width="{bw}" '
             f'height="{H-HORIZON}" fill="url(#beamG)" opacity="{cfg["beam"][1]:.2f}" filter="url(#reflBlur)"/></g>')
    s.append(f'<g clip-path="url(#waterClip)"><ellipse cx="{ox}" cy="{HORIZON+16}" rx="{orr*1.3:.0f}" '
             f'ry="{orr*2.0:.0f}" fill="{cfg["core"][1]}" opacity="0.35" filter="url(#reflBlur)"/></g>')
    s.append(ripples(cfg['ripd'], 21, 9, 0.5))
    s.append(ripples(cfg['ripl'], 22, 4, 0.55))
    s.append(boat(880 if night else 2360, 266 if night else 262, cfg['bank']))

    # 飞鸟
    for i in range(cfg['birds']):
        bx, by, bs = BIRD_POS[(i + (2 if night else 0)) % len(BIRD_POS)]
        s.append(bird(bx, by, bs, cfg['birdc'], cfg['birdop']))

    s.append(seal(2836, 236, 'moon' if night else 'sun', cfg['sealbg'], cfg['sealfg']))
    s.append(f'<rect x="6" y="6" width="{W-12}" height="{H-12}" fill="none" stroke="{cfg["frame"]}" '
             f'stroke-width="2" opacity="{cfg["frameop"]}"/>')
    s.append('</svg>')
    return "\n".join(s)


import os
OUT = '/data/workspace/日月山河十二帧'
os.makedirs(OUT, exist_ok=True)
for cfg in F:
    path = f"{OUT}/{cfg['no']}_{cfg['name']}.svg"
    with open(path, 'w', encoding='utf-8') as f:
        f.write(build(cfg))
    print('ok', path)
