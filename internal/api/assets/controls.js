/* Progressive desktop enhancement. The native select remains the form source of truth. */
(() => {
  'use strict';
  const desktop = matchMedia('(hover: hover) and (pointer: fine)');
  const reducedMotion = matchMedia('(prefers-reduced-motion: reduce)');
  let openControl = null;
  let sequence = 0;
  let pendingConfirmation = null;
  let confirmationOrigin = null;

  function close(control, restoreFocus = false) {
    if (!control || !control.open) return;
    control.open = false;
    control.button.setAttribute('aria-expanded', 'false');
    control.button.removeAttribute('aria-activedescendant');
    control.popup.setAttribute('aria-hidden', 'true');
    control.popup.classList.add('is-closing');
    const finish = () => {
      control.popup.hidden = true;
      control.popup.remove();
      control.popup.classList.remove('is-closing');
      control.closeTimer = null;
    };
    if (reducedMotion.matches) finish();
    else control.closeTimer = window.setTimeout(finish, 110);
    if (openControl === control) openControl = null;
    if (restoreFocus && control.button.isConnected) control.button.focus();
  }

  function options(control) {
    return Array.from(control.select.options).filter(option => !option.disabled && option.value !== '');
  }

  function sync(control) {
    const selected = control.select.selectedOptions[0];
    control.button.querySelector('.glass-select-value').textContent = selected ? selected.textContent.trim() : '';
    control.button.classList.toggle('is-placeholder', !control.select.value);
    control.button.disabled = control.select.disabled;
    control.button.setAttribute('aria-invalid', control.select.getAttribute('aria-invalid') || 'false');
    control.popup.querySelectorAll('[role="option"]').forEach(item => {
      item.setAttribute('aria-selected', item.dataset.value === control.select.value ? 'true' : 'false');
    });
  }

  function position(control) {
    if (!control.open) return;
    const rect = control.button.getBoundingClientRect();
    const margin = 10;
    const below = window.innerHeight - rect.bottom - margin;
    const above = rect.top - margin;
    const upward = below < Math.min(250, control.popup.scrollHeight) && above > below;
    const room = Math.max(90, upward ? above : below);
    const width = Math.min(rect.width, window.innerWidth - margin * 2);
    control.popup.style.width = `${width}px`;
    control.popup.style.maxHeight = `${Math.min(320, room)}px`;
    control.popup.style.left = `${Math.max(margin, Math.min(rect.left, window.innerWidth - width - margin))}px`;
    control.popup.style.top = upward ? 'auto' : `${rect.bottom + 5}px`;
    control.popup.style.bottom = upward ? `${window.innerHeight - rect.top + 5}px` : 'auto';
    control.popup.dataset.placement = upward ? 'above' : 'below';
  }

  function active(control, option) {
    if (!option) return;
    control.activeValue = option.value;
    const item = Array.from(control.popup.children).find(node => node.dataset.value === option.value);
    control.popup.querySelectorAll('[role="option"]').forEach(node => node.classList.toggle('is-active', node === item));
    if (item) {
      control.button.setAttribute('aria-activedescendant', item.id);
      item.scrollIntoView({block: 'nearest'});
    }
  }

  function open(control) {
    if (control.button.disabled) return;
    if (openControl && openControl !== control) close(openControl);
    if (control.closeTimer) window.clearTimeout(control.closeTimer);
    control.closeTimer = null;
    control.popup.classList.remove('is-closing');
    document.body.append(control.popup);
    control.popup.hidden = false;
    control.popup.removeAttribute('aria-hidden');
    control.open = true;
    openControl = control;
    control.button.setAttribute('aria-expanded', 'true');
    position(control);
    const available = options(control);
    active(control, available.find(option => option.value === control.select.value) || available[0]);
  }

  function choose(control, value) {
    const option = options(control).find(item => item.value === value);
    if (!option) return;
    control.select.value = option.value;
    control.select.setAttribute('aria-invalid', 'false');
    control.select.dispatchEvent(new Event('input', {bubbles: true}));
    control.select.dispatchEvent(new Event('change', {bubbles: true}));
    control.error.hidden = true;
    control.button.removeAttribute('aria-invalid');
    sync(control);
    close(control, true);
  }

  function enhance(select) {
    if (select.dataset.glassReady || !desktop.matches) return;
    select.dataset.glassReady = 'true';
    const id = `glass-select-${++sequence}`;
    const label = select.id ? document.querySelector(`label[for="${CSS.escape(select.id)}"]`) : null;
    if (label && !label.id) label.id = `${id}-label`;
    const wrapper = document.createElement('span');
    wrapper.className = 'glass-select';
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'glass-select-trigger';
    button.setAttribute('role', 'combobox');
    button.setAttribute('aria-haspopup', 'listbox');
    button.setAttribute('aria-expanded', 'false');
    button.setAttribute('aria-controls', `${id}-list`);
    if (label) button.setAttribute('aria-labelledby', label.id);
    else button.setAttribute('aria-label', select.getAttribute('aria-label') || select.name);
    if (select.getAttribute('aria-describedby')) button.setAttribute('aria-describedby', select.getAttribute('aria-describedby'));
    const value = document.createElement('span');
    value.className = 'glass-select-value';
    value.id = `${id}-value`;
    if (label) button.setAttribute('aria-labelledby', `${label.id} ${value.id}`);
    const chevron = document.createElement('span');
    chevron.className = 'glass-select-chevron';
    chevron.setAttribute('aria-hidden', 'true');
    chevron.textContent = '⌄';
    button.append(value, chevron);
    const popup = document.createElement('div');
    popup.id = `${id}-list`;
    popup.className = 'glass-select-list';
    popup.setAttribute('role', 'listbox');
    popup.setAttribute('aria-label', label ? label.textContent.trim() : select.getAttribute('aria-label') || select.name);
    popup.hidden = true;
    Array.from(select.options).forEach((option, index) => {
      const item = document.createElement('div');
      item.id = `${id}-option-${index}`;
      item.setAttribute('role', 'option');
      item.dataset.value = option.value;
      item.textContent = option.textContent;
      item.setAttribute('aria-disabled', option.disabled ? 'true' : 'false');
      if (option.disabled) item.classList.add('is-disabled');
      popup.append(item);
    });
    const error = document.createElement('span');
    error.className = 'field-error';
    error.id = `${id}-error`;
    error.setAttribute('role', 'alert');
    error.hidden = true;
    select.classList.add('enhanced-native-select');
    select.tabIndex = -1;
    select.setAttribute('aria-hidden', 'true');
    select.before(wrapper);
    wrapper.append(select, button, error);
    const control = {select, button, popup, error, open: false, activeValue: ''};
    sync(control);
    button.addEventListener('click', () => control.open ? close(control) : open(control));
    button.addEventListener('keydown', event => {
      const available = options(control);
      if (event.key === 'Escape' && control.open) { event.preventDefault(); close(control, true); return; }
      if (event.key === 'Tab') { close(control); return; }
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp' || event.key === 'Home' || event.key === 'End') {
        event.preventDefault();
        if (!control.open) open(control);
        if (!available.length) return;
        const current = available.findIndex(item => item.value === control.activeValue);
        const next = event.key === 'Home' ? 0 : event.key === 'End' ? available.length - 1 :
          event.key === 'ArrowDown' ? Math.min(available.length - 1, current + 1) : current < 0 ? available.length - 1 : Math.max(0, current - 1);
        active(control, available[next]);
      } else if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        if (control.open) choose(control, control.activeValue);
        else open(control);
      }
    });
    popup.addEventListener('click', event => {
      const item = event.target.closest('[role="option"]');
      if (item && item.getAttribute('aria-disabled') !== 'true') choose(control, item.dataset.value);
    });
    popup.addEventListener('pointermove', event => {
      const item = event.target.closest('[role="option"]');
      if (item && item.getAttribute('aria-disabled') !== 'true') {
        const option = options(control).find(candidate => candidate.value === item.dataset.value);
        active(control, option);
      }
    });
    select.addEventListener('change', () => sync(control));
    select.addEventListener('invalid', event => {
      event.preventDefault();
      control.error.textContent = select.dataset.requiredMessage || 'Choose an option.';
      control.error.hidden = false;
      button.setAttribute('aria-invalid', 'true');
      button.setAttribute('aria-describedby', [select.getAttribute('aria-describedby'), error.id].filter(Boolean).join(' '));
      button.focus();
    });
    if (label) label.addEventListener('click', event => { event.preventDefault(); button.focus(); });
  }

  function scan(root = document) { root.querySelectorAll('select:not([data-native-select])').forEach(enhance); }
  function resetSubmission(form) {
    if (!(form instanceof HTMLFormElement)) return;
    delete form.dataset.pending;
    delete form.dataset.confirmed;
    form.querySelectorAll('button[aria-busy="true"]').forEach(button => button.removeAttribute('aria-busy'));
  }

  function setupDialog() {
    const dialog = document.getElementById('action-confirmation');
    if (!dialog || typeof dialog.showModal !== 'function') return;
    const cancel = dialog.querySelector('[data-dialog-cancel]');
    const accept = dialog.querySelector('[data-dialog-accept]');
    cancel.addEventListener('click', () => dialog.close());
    dialog.addEventListener('close', () => {
      pendingConfirmation = null;
      if (confirmationOrigin && confirmationOrigin.isConnected) confirmationOrigin.focus();
      confirmationOrigin = null;
    });
    accept.addEventListener('click', () => {
      if (!pendingConfirmation) return;
      const {form, submitter} = pendingConfirmation;
      pendingConfirmation = null;
      confirmationOrigin = null;
      dialog.close();
      if (!form.isConnected) return;
      form.dataset.confirmed = 'true';
      if (submitter) form.requestSubmit(submitter);
      else form.requestSubmit();
    });
  }

  document.addEventListener('submit', event => {
    const form = event.target;
    if (!(form instanceof HTMLFormElement) || form.method.toLowerCase() !== 'post') return;
    if (form.dataset.pending === 'true') { event.preventDefault(); return; }
    const dialog = document.getElementById('action-confirmation');
    if (form.dataset.confirmTitle && form.dataset.confirmed !== 'true' && dialog && typeof dialog.showModal === 'function') {
      event.preventDefault();
      const submitter = event.submitter instanceof HTMLElement ? event.submitter : null;
      pendingConfirmation = {form, submitter};
      confirmationOrigin = submitter;
      dialog.querySelector('#action-confirmation-title').textContent = form.dataset.confirmTitle;
      dialog.querySelector('#action-confirmation-body').textContent = form.dataset.confirmBody || '';
      const target = dialog.querySelector('#action-confirmation-target');
      target.hidden = !form.dataset.confirmTarget;
      target.querySelector('code').textContent = form.dataset.confirmTarget || '';
      dialog.querySelector('[data-dialog-accept]').textContent = form.dataset.confirmAction || form.dataset.confirmTitle;
      dialog.dataset.level = form.dataset.confirmLevel || 'standard';
      dialog.showModal();
      dialog.querySelector('[data-dialog-cancel]').focus();
      return;
    }
    form.dataset.pending = 'true';
    const submitter = event.submitter instanceof HTMLElement ? event.submitter : form.querySelector('button[type="submit"],button:not([type])');
    if (submitter) submitter.setAttribute('aria-busy', 'true');
  });

  document.addEventListener('pointerdown', event => {
    document.querySelectorAll('.account-menu[open], .theme-menu[open]').forEach(menu => {
      if (!menu.contains(event.target)) menu.open = false;
    });
  });
  document.addEventListener('keydown', event => {
    if (event.key !== 'Escape') return;
    const menu = event.target.closest('.account-menu[open], .theme-menu[open]');
    if (menu) { event.preventDefault(); menu.open = false; menu.querySelector('summary').focus(); }
  });

  function runRequestSource(event) {
    const source = event.detail && event.detail.elt;
    return source instanceof Element && source.closest('#runs-content');
  }
  document.addEventListener('htmx:beforeRequest', event => {
    const content = runRequestSource(event);
    if (!content) return;
    content.classList.add('is-loading');
    content.querySelector('#runs-request-status')?.classList.remove('has-error', 'is-auth-error');
  });
  document.addEventListener('htmx:afterRequest', event => {
    const form = event.detail && event.detail.elt;
    if (form instanceof HTMLFormElement) resetSubmission(form);
    if (!runRequestSource(event)) return;
    const content = document.getElementById('runs-content');
    content?.classList.remove('is-loading');
    if (event.detail.failed) {
      content?.querySelector('#runs-request-status')?.classList.add(event.detail.xhr?.status === 401 ? 'is-auth-error' : 'has-error');
    }
  });
  for (const name of ['htmx:sendError', 'htmx:timeout']) {
    document.addEventListener(name, event => {
      if (!runRequestSource(event)) return;
      const content = document.getElementById('runs-content');
      content?.classList.remove('is-loading');
      content?.querySelector('#runs-request-status')?.classList.add('has-error');
    });
  }
  window.addEventListener('pageshow', () => document.querySelectorAll('form[data-pending="true"]').forEach(resetSubmission));
  document.addEventListener('pointerdown', event => {
    if (openControl && !openControl.button.contains(event.target) && !openControl.popup.contains(event.target)) close(openControl);
  });
  window.addEventListener('resize', () => position(openControl));
  window.addEventListener('scroll', () => position(openControl), true);
  document.addEventListener('htmx:beforeSwap', () => close(openControl));
  document.addEventListener('htmx:afterSwap', event => scan(event.target));
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => { scan(); setupDialog(); });
  else { scan(); setupDialog(); }
})();
