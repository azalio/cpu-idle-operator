# Wayfinding Handoff: Кодирование ортогональных CPU-тиров

- **Slug:** `cpu-idle-tiers`
- **Generated:** 2026-08-16T18:51:27.344911Z
- **Early handoff:** no

## Destination

Поправка к замороженной карте cpu-idle-operator: idle и burst — ортогональные переключатели, а не значения одного тира. Определить кодирование в аннотациях и уточнить, какие соседние knob'ы каждый из них реально блокирует.

## Decisions Made

| # | Question | Decision | Resolution |
|---|----------|----------|------------|
| 1 | Раз под может быть одновременно idle и burst, одна аннотация с одним значением их не выражает — какими ключами кодируем два независимых переключателя, сохраняя уже согласованный для деки cpu.azalio.net/tier: idle? | Два независимых ключа: cpu.azalio.net/tier: idle (порядок выбора) и cpu.azalio.net/burst (ширина полосы), под может нести оба — измерено, что idle, cpu.max и cpu.max.burst сосуществуют на одной cgroup без конфликта; поправка к прежнему выводу: idle запирает только cpu.weight, burst — только понижение quota, крест-накрест блокировок нет | [T-001](resolutions/T-001.md) |

## Out of Scope

_None._

## Open Questions / Remaining Risks

- Асимметрия имён ключей (tier: idle против burst: "true") оставлена сознательно ради уже согласованного с деками cpu.azalio.net/tier; обратима до публикации README
- Числовое значение ключа burst в первой версии не парсится — формат переопределения величины burst не зафиксирован
- Ортогональность измерена на голой cgroup, а не на настоящем pod-cgroup под kubelet: поведение комбинации idle+burst при рестарте kubelet и цикле UpdateCgroups не проверялось

---

Feed this into planning with `/map-plan --wayfind cpu-idle-tiers` on a feature branch.
