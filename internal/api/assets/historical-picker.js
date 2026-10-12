/* Progressive UTC historical-window picker. The server remains authoritative. */
(() => {
  'use strict';

  const dayMs = 24 * 60 * 60 * 1000;
  const preciseUTC = (day, clock) => {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(day) || !/^(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d$/.test(clock)) return null;
    const value = `${day}T${clock}Z`;
    const instant = new Date(value);
    return Number.isFinite(instant.getTime()) && `${instant.toISOString().slice(0, 19)}Z` === value ? value : null;
  };
  const permitted = (from, to, now) => {
    const start = preciseUTC(from.day, from.clock);
    const end = preciseUTC(to.day, to.clock);
    return !!start && !!end && Date.parse(start) < Date.parse(end) &&
      Date.parse(start) >= now - 29 * dayMs && Date.parse(end) <= now - 2 * 60 * 1000;
  };

  if (typeof module !== 'undefined' && module.exports) module.exports = {preciseUTC, permitted};
  if (typeof document === 'undefined' || !window.matchMedia('(min-width: 640px) and (hover: hover) and (pointer: fine)').matches ||
      !window.VanillaCalendarPro?.Calendar) return;

  const labelsRU = {
    application: 'Календарь', navigation: 'Навигация по календарю',
    arrowNext: {month: 'Следующий месяц', year: 'Следующие годы', week: 'Следующая неделя'},
    arrowPrev: {month: 'Предыдущий месяц', year: 'Предыдущие годы', week: 'Предыдущая неделя'},
    month: 'Выбрать месяц, текущий:', months: 'Список месяцев',
    year: 'Выбрать год, текущий:', years: 'Список годов',
    week: 'Дни недели', dates: 'Даты текущего месяца',
  };

  const form = document.querySelector('.reconcile-form');
  if (!form) return;
  const fields = {};
  for (const side of ['from', 'to']) {
    const original = form.querySelector(`#reconcile-${side}`);
    if (!original) return;
    const initial = original.value;
    const date = document.createElement('input');
    date.type = 'text';
    date.id = `reconcile-${side}-date`;
    date.className = 'historical-date';
    date.readOnly = true;
    date.required = true;
    date.value = initial.slice(0, 10);
    date.placeholder = 'YYYY-MM-DD';
    date.autocomplete = 'off';
    const clock = document.createElement('input');
    clock.type = 'text';
    clock.id = `reconcile-${side}-time`;
    clock.className = 'historical-time';
    clock.required = true;
    clock.inputMode = 'numeric';
    clock.pattern = '(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]';
    clock.placeholder = 'HH:MM:SS';
    clock.autocomplete = 'off';
    clock.value = initial.slice(11, 19);
    const hint = document.createElement('small');
    hint.id = `reconcile-${side}-picker-hint`;
    hint.className = 'field-help';
    hint.textContent = original.dataset.calendarHint;
    date.setAttribute('aria-describedby', hint.id);
    clock.setAttribute('aria-describedby', hint.id);
    const row = document.createElement('div');
    row.className = 'historical-picker-row';
    for (const [input, text] of [[date, original.dataset.dateLabel], [clock, original.dataset.timeLabel]]) {
      const label = document.createElement('label');
      label.htmlFor = input.id;
      label.textContent = text;
      const item = document.createElement('div');
      item.className = 'historical-picker-part';
      item.append(label, input);
      row.append(item);
    }
    const wrapper = document.createElement('div');
    wrapper.className = 'historical-picker';
    wrapper.append(row, hint);
    const sync = () => {
      const value = preciseUTC(date.value, clock.value);
      original.value = value ? value.slice(0, -1) : '';
      date.setCustomValidity('');
      clock.setCustomValidity('');
    };
    const selected = () => {
      const picked = calendar.context.selectedDates[0];
      if (picked) {
        queueMicrotask(() => { date.value = picked; sync(); calendar.hide(); date.focus(); });
      }
    };
    const bounds = {min: new Date(Date.now() - 29 * dayMs).toISOString().slice(0, 10),
      max: new Date(Date.now() - 2 * 60 * 1000).toISOString().slice(0, 10)};
    let calendar;
    try {
      original.after(wrapper);
      calendar = new window.VanillaCalendarPro.Calendar(date, {
        inputMode: true, openOnFocus: true, positionToInput: 'auto',
        locale: document.documentElement.lang === 'ru' ? 'ru' : 'en',
        ...(document.documentElement.lang === 'ru' ? {labels: labelsRU} : {}),
        selectionDatesMode: 'single', enableDateToggle: false, enableJumpToSelectedDate: true,
        dateMin: bounds.min, dateMax: bounds.max,
        ...(date.value ? {selectedDates: [date.value]} : {}),
        onClickDate: selected, onChangeToInput: selected,
      });
      calendar.init();
    } catch (error) {
      wrapper.remove();
      continue; // Keep the native datetime-local field when enhancement cannot initialize.
    }
    original.id = `reconcile-${side}-value`;
    original.type = 'hidden';
    const outerLabel = form.querySelector(`label[for="reconcile-${side}"]`);
    if (outerLabel) outerLabel.htmlFor = date.id;
    date.value = initial.slice(0, 10);
    date.addEventListener('change', sync);
    clock.addEventListener('input', sync);
    date.addEventListener('keydown', event => {
      if (event.key === 'Escape') { calendar.hide(); date.focus(); }
    });
    clock.addEventListener('keydown', event => {
      if (event.key === 'Escape') { calendar.hide(); date.focus(); }
    });
    sync();
    fields[side] = {date, clock, original};
  }
  if (!fields.from || !fields.to) return;
  form.addEventListener('submit', event => {
    const from = {day: fields.from.date.value, clock: fields.from.clock.value};
    const to = {day: fields.to.date.value, clock: fields.to.clock.value};
    const valid = permitted(from, to, Date.now());
    if (valid) return;
    event.preventDefault();
    const target = !preciseUTC(from.day, from.clock) ? fields.from.clock :
      !preciseUTC(to.day, to.clock) ? fields.to.clock : fields.to.clock;
    target.setCustomValidity(target === fields.to.clock ? fields.to.original.dataset.pickerInvalid : fields.from.original.dataset.pickerInvalid);
    target.reportValidity();
  });
})();
