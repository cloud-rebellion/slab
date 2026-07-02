import { apiDelete, apiGet, apiPut } from './client';
import type { SiteSetting } from './types';

export type SettingsCategory =
	| 'security'
	| 'seo'
	| 'analytics'
	| 'general'
	| 'design'
	| 'search'
	| 'payments'
	| 'wizard_state'
	| 'server';

export interface SettingUpsertInput {
	category: string;
	key: string;
	value: string;
}

// CookieDeclaration mirrors internal/builder/cookieproof.go CookieDeclaration.
// Used by the Cookies admin page to render the merged preset + user-edited
// declaration table.
export interface CookieDeclaration {
	category: string;
	name: string;
	provider?: string;
	purpose?: string;
	expiry?: string;
}

// CookieTranslation is one language's worth of overridable banner copy.
// All fields optional; the widget falls back to its built-in translations
// for any field left empty. Mirrors the en.ts/sv.ts/etc. shape in
// CookieProof/src/i18n/.
export interface CookieTranslation {
	title?: string;
	description?: string;
	accept?: string;
	reject?: string;
	customize?: string;
}

// COOKIE_LANGUAGES is the list of locales the embedded CookieProof widget
// ships translations for (CookieProof/src/i18n/index.ts). The admin lets
// the operator pick a subset for the language-selector dropdown and
// override the strings per language.
export const COOKIE_LANGUAGES: { code: string; label: string }[] = [
	{ code: 'en', label: 'English' },
	{ code: 'sv', label: 'Svenska' },
	{ code: 'de', label: 'Deutsch' },
	{ code: 'fr', label: 'Français' },
	{ code: 'es', label: 'Español' },
	{ code: 'nl', label: 'Nederlands' },
	{ code: 'no', label: 'Norsk' },
	{ code: 'da', label: 'Dansk' },
	{ code: 'fi', label: 'Suomi' },
	{ code: 'pt', label: 'Português' },
	{ code: 'it', label: 'Italiano' },
	{ code: 'pl', label: 'Polski' },
	{ code: 'ja', label: '日本語' }
];

// COOKIE_BUILTIN_TRANSLATIONS shows the operator the bundled string for
// each language so they can leave fields empty (use built-in) or override.
// Mirrors CookieProof/src/i18n/{en,sv,...}.ts, kept in sync manually.
// Strings here only represent the 5 admin-overridable keys. Other modal
// strings (categories, table headers, etc.) are not overridable.
export const COOKIE_BUILTIN_TRANSLATIONS: Record<string, Required<CookieTranslation>> = {
	en: {
		title: 'We respect your privacy',
		description:
			'We use cookies to improve your experience. You can choose which categories to accept.',
		accept: 'Accept all',
		reject: 'Reject all',
		customize: 'Settings'
	},
	sv: {
		title: 'Vi respekterar din integritet',
		description:
			'Vi använder cookies för att förbättra din upplevelse. Du kan välja vilka kategorier du godkänner.',
		accept: 'Godkänn alla',
		reject: 'Avvisa alla',
		customize: 'Inställningar'
	},
	de: {
		title: 'Wir respektieren Ihre Privatsphäre',
		description:
			'Wir verwenden Cookies, um Ihre Erfahrung zu verbessern. Sie können wählen, welche Kategorien Sie akzeptieren.',
		accept: 'Alle akzeptieren',
		reject: 'Alle ablehnen',
		customize: 'Einstellungen'
	},
	fr: {
		title: 'Nous respectons votre vie privée',
		description:
			"Nous utilisons des cookies pour améliorer votre expérience. Vous pouvez choisir les catégories à accepter.",
		accept: 'Tout accepter',
		reject: 'Tout refuser',
		customize: 'Paramètres'
	},
	es: {
		title: 'Respetamos su privacidad',
		description:
			'Usamos cookies para mejorar su experiencia. Puede elegir qué categorías aceptar.',
		accept: 'Aceptar todo',
		reject: 'Rechazar todo',
		customize: 'Configuración'
	},
	nl: {
		title: 'Wij respecteren uw privacy',
		description:
			'We gebruiken cookies om uw ervaring te verbeteren. U kunt kiezen welke categorieën u accepteert.',
		accept: 'Alles accepteren',
		reject: 'Alles weigeren',
		customize: 'Instellingen'
	},
	no: {
		title: 'Vi respekterer ditt personvern',
		description:
			'Vi bruker informasjonskapsler for å forbedre opplevelsen din. Du kan velge hvilke kategorier du godtar.',
		accept: 'Godta alle',
		reject: 'Avvis alle',
		customize: 'Innstillinger'
	},
	da: {
		title: 'Vi respekterer dit privatliv',
		description:
			'Vi bruger cookies til at forbedre din oplevelse. Du kan vælge, hvilke kategorier du accepterer.',
		accept: 'Accepter alle',
		reject: 'Afvis alle',
		customize: 'Indstillinger'
	},
	fi: {
		title: 'Kunnioitamme yksityisyyttäsi',
		description:
			'Käytämme evästeitä parantaaksemme kokemustasi. Voit valita, mitkä luokat hyväksyt.',
		accept: 'Hyväksy kaikki',
		reject: 'Hylkää kaikki',
		customize: 'Asetukset'
	},
	pt: {
		title: 'Respeitamos sua privacidade',
		description:
			'Usamos cookies para melhorar sua experiência. Você pode escolher quais categorias aceitar.',
		accept: 'Aceitar tudo',
		reject: 'Rejeitar tudo',
		customize: 'Configurações'
	},
	it: {
		title: 'Rispettiamo la tua privacy',
		description:
			"Utilizziamo i cookie per migliorare la tua esperienza. Puoi scegliere quali categorie accettare.",
		accept: 'Accetta tutto',
		reject: 'Rifiuta tutto',
		customize: 'Impostazioni'
	},
	pl: {
		title: 'Szanujemy Twoją prywatność',
		description:
			'Używamy plików cookie, aby ulepszyć Twoje doświadczenie. Możesz wybrać, które kategorie zaakceptować.',
		accept: 'Zaakceptuj wszystkie',
		reject: 'Odrzuć wszystkie',
		customize: 'Ustawienia'
	},
	ja: {
		title: 'プライバシーを尊重します',
		description: '体験を向上させるためにCookieを使用します。受け入れるカテゴリを選択できます。',
		accept: 'すべて承認',
		reject: 'すべて拒否',
		customize: '設定'
	}
};

export function list(siteID: string): Promise<SiteSetting[]> {
	return apiGet<SiteSetting[]>(`/sites/${siteID}/settings`);
}

export function listByCategory(siteID: string, category: string): Promise<SiteSetting[]> {
	return apiGet<SiteSetting[]>(`/sites/${siteID}/settings/${category}`);
}

export function upsert(siteID: string, input: SettingUpsertInput): Promise<SiteSetting> {
	return apiPut<SiteSetting>(`/sites/${siteID}/settings`, input);
}

export function bulkUpsert(siteID: string, items: SettingUpsertInput[]): Promise<SiteSetting[]> {
	return apiPut<SiteSetting[]>(`/sites/${siteID}/settings/bulk`, items);
}

export function remove(siteID: string, settingID: string): Promise<{ status: string }> {
	return apiDelete<{ status: string }>(`/sites/${siteID}/settings/${settingID}`);
}

// getCookiePresets returns the cookie declarations atomicsite auto-derives
// from the site's enabled trackers (GA4, language settings, etc.). The
// admin Cookies page renders these alongside the user-edited list so
// the operator sees the full disclosure surface without re-entering
// preset data. Computed on the fly server-side from current settings.
export function getCookiePresets(siteID: string): Promise<CookieDeclaration[]> {
	return apiGet<CookieDeclaration[]>(`/sites/${siteID}/settings/cookie-presets`);
}
