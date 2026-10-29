(() => {
  const translations = {
    en: {
      title:'Sign in · PrismVox', eyebrow:'VOICE & TELEPHONY', headline1:'Every call', headline2:'matters.',
      description:'Your agent picks up. You stay in the loop.', signal:'A voice. A conversation. A connection.',
      caption:'Always an attentive ear.', console:'YOUR CONSOLE', welcome:'Welcome back.',
      intro:'Sign in to manage your reception.', username:'Username', password:'Password', submit:'Sign in',
      help:'Use the credentials for your PrismVox instance.', footer:'Your reception, connected.', source:'Open source · MIT',
      show:'Show password', hide:'Hide password', theme:'Switch theme', busy:'Signing in…',
      credentials:'Incorrect username or password.', rate_limit:'Too many attempts. Please try again in a minute.',
      network:'Unable to connect. Please try again.'
    },
    fr: {
      title:'Connexion · PrismVox', eyebrow:'VOIX & TÉLÉPHONIE', headline1:'Chaque appel', headline2:'compte.',
      description:'Votre agent décroche. Vous gardez le fil.', signal:'Une voix. Un échange. Un lien.',
      caption:'Toujours une oreille attentive.', console:'VOTRE CONSOLE', welcome:'Bienvenue.',
      intro:'Connectez-vous pour retrouver votre standard.', username:'Identifiant', password:'Mot de passe', submit:'Se connecter',
      help:'Utilisez les identifiants de votre instance PrismVox.', footer:'Votre standard, connecté.', source:'Open source · MIT',
      show:'Afficher le mot de passe', hide:'Masquer le mot de passe', theme:'Changer de thème', busy:'Connexion…',
      credentials:'Identifiant ou mot de passe incorrect.', rate_limit:'Trop de tentatives. Réessayez dans une minute.',
      network:'Connexion impossible. Veuillez réessayer.'
    }
  };
  const saved = key => { try { return localStorage.getItem(key); } catch { return null; } };
  const save = (key, value) => { try { localStorage.setItem(key, value); } catch {} };
  let language = saved('vox-language') || (navigator.language.startsWith('fr') ? 'fr' : 'en');
  if (!translations[language]) language = 'en';
  const root = document.documentElement;
  root.dataset.theme = saved('theme') === 'light' || (!saved('theme') && matchMedia('(prefers-color-scheme: light)').matches) ? 'light' : 'dark';
  const form = document.getElementById('login-form');
  const password = document.getElementById('password');
  const toggle = document.getElementById('password-toggle');
  const status = document.getElementById('login-status');
  let busy = false;
  let error = status.dataset.error;

  function render() {
    const t = translations[language];
    root.lang = language;
    document.title = t.title;
    document.querySelectorAll('[data-i18n]').forEach(el => { el.textContent = t[el.dataset.i18n]; });
    document.getElementById('language-toggle').textContent = language === 'fr' ? 'EN' : 'FR';
    document.getElementById('language-toggle').setAttribute('aria-label', language === 'fr' ? 'English' : 'Français');
    document.getElementById('theme-toggle').setAttribute('aria-label', t.theme);
    toggle.setAttribute('aria-label', password.type === 'password' ? t.show : t.hide);
    status.textContent = error ? t[error] || t.network : '';
    form.querySelector('.submit-label').textContent = busy ? t.busy : t.submit;
  }
  render();
  toggle.hidden = false;
  document.getElementById('language-toggle').addEventListener('click', () => {
    language = language === 'fr' ? 'en' : 'fr'; save('vox-language', language); render();
  });
  document.getElementById('theme-toggle').addEventListener('click', () => {
    root.dataset.theme = root.dataset.theme === 'dark' ? 'light' : 'dark'; save('theme', root.dataset.theme);
  });
  toggle.addEventListener('click', () => {
    password.type = password.type === 'password' ? 'text' : 'password';
    toggle.setAttribute('aria-pressed', String(password.type === 'text')); render();
  });
  form.addEventListener('input', () => {
    if (!error) return;
    error = ''; form.querySelectorAll('[aria-invalid]').forEach(el => el.removeAttribute('aria-invalid')); render();
  });
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (busy) return;
    busy = true; error = ''; form.setAttribute('aria-busy', 'true');
    form.querySelector('[type="submit"]').disabled = true; render();
    try {
      const response = await fetch(form.action, {method:'POST', body:new FormData(form), headers:{Accept:'application/json'}});
      if (response.ok) { window.location.replace('/'); return; }
      const result = await response.json().catch(() => ({}));
      error = result.error || 'network';
      if (response.status === 401) {
        password.value = ''; password.setAttribute('aria-invalid', 'true'); password.focus();
      }
    } catch { error = 'network'; }
    busy = false; form.removeAttribute('aria-busy'); form.querySelector('[type="submit"]').disabled = false; render();
  });
})();
