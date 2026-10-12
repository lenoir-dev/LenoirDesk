package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const languageCookie = "lenoirdesk_lang"

// Only the two reviewed interface languages are accepted. The cookie affects
// presentation, never tenant selection, authorization or business behavior.
func (a *application) pageLanguage(w http.ResponseWriter, r *http.Request) string {
	if selected := r.URL.Query().Get("lang"); selected == "en" || selected == "ru" {
		a.setCookie(w, languageCookie, selected, "/", 365*24*60*60)
		return selected
	}
	if cookie, err := r.Cookie(languageCookie); err == nil && cookie.Value == "ru" {
		return "ru"
	}
	return "en"
}

func languageURL(r *http.Request, lang string) string {
	if lang != "en" && lang != "ru" {
		lang = "en"
	}
	query := r.URL.Query()
	query.Set("lang", lang)
	path := r.URL.Path
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		path = "/"
	}
	return (&url.URL{Path: path, RawQuery: query.Encode()}).String()
}

var interfaceCopy = map[string][2]string{
	"stripe_setup_title":             {"Connect Stripe Test Mode", "Подключить Stripe в тестовом режиме"},
	"stripe_setup_intro":             {"Use a restricted rk_test_ key. LenoirDesk checks the account and read access, then encrypts the key and webhook signing secret. Historical checks require Events Read. These values will not be shown again.", "Используйте ключ с ограниченными правами rk_test_. LenoirDesk проверит аккаунт и доступ на чтение, затем зашифрует ключ и секрет подписи webhook. Для проверки прошлых событий нужен доступ Events Read. После сохранения значения больше не показываются."},
	"stripe_setup_destination":       {"Create a Stripe Test Mode destination for these snapshot events:", "Создайте тестовое назначение Stripe для следующих snapshot-событий:"},
	"stripe_setup_tunnel":            {"If Stripe cannot reach localhost, use an HTTPS tunnel only for this test. Copy the destination signing secret; a Stripe CLI secret will not work.", "Если Stripe не может обратиться к localhost, используйте HTTPS-туннель только для этого теста. Скопируйте секрет подписи назначения; секрет Stripe CLI не подойдёт."},
	"stripe_setup_path_title":        {"Destination path for this connection", "Путь назначения для этого подключения"},
	"stripe_setup_path_help":         {"Use this path under the test HTTPS origin. The path only routes delivery; the signature still protects the payload. If you leave this page, generate a new path and update the destination before submitting.", "Используйте этот путь с тестовым HTTPS-адресом. Путь только направляет доставку; данные защищает проверка подписи. Если вы покинете страницу, создайте новый путь и обновите назначение перед отправкой формы."},
	"stripe_setup_credentials_title": {"Verify and save credentials", "Проверить и сохранить данные доступа"},
	"stripe_setup_account":           {"Expected Stripe account ID", "Ожидаемый ID аккаунта Stripe"},
	"stripe_setup_account_help":      {"A key from another account will be rejected.", "Ключ другого аккаунта будет отклонён."},
	"stripe_setup_restricted_key":    {"Restricted Test Mode API key", "Ключ API с ограниченными правами для тестового режима"},
	"stripe_setup_signing_secret":    {"Destination webhook signing secret", "Секрет подписи webhook-назначения"},
	"stripe_setup_submit":            {"Verify and connect", "Проверить и подключить"},
	"demo_connections_readonly":      {"Synthetic demo connections are fixed. Use the reset command to start a fresh scenario.", "Подключения в синтетическом демо фиксированы. Чтобы начать сценарий заново, выполните команду сброса."},
	"timeline_delivery_confirmed":    {"Delivery confirmed by operator", "Доставку подтвердил оператор"},
	"timeline_manual_retry":          {"CRM retry requested by operator", "Оператор запросил повтор CRM"},
	"timeline_risk_retry":            {"Retry requested with duplicate risk acknowledged", "Повтор запрошен с подтверждённым риском дубля"},
	"timeline_leave_unknown":         {"Unknown delivery left unresolved", "Неизвестная доставка оставлена без решения"},
	"audit_success":                  {"completed", "выполнено"},
	"Overview":                       {"Overview", "Обзор"},
	"Connections":                    {"Connections", "Подключения"},
	"Automation":                     {"Automation", "Автоматизация"},
	"Runs":                           {"Runs", "Запуски"},
	"Run details":                    {"Run details", "Детали запуска"},
	"Reconciliation":                 {"Reconciliation", "Сверка"},
	"Contact mapping":                {"Contact mapping", "Связь контакта"},
	"workspace":                      {"Workspace", "Рабочая область"},
	"workspaces":                     {"Workspaces", "Рабочие области"},
	"change_workspace":               {"Change workspace", "Сменить область"},
	"current_workspace":              {"Current workspace", "Текущая область"},
	"workspace_navigation":           {"Workspace navigation", "Навигация по области"},
	"account":                        {"Account", "Аккаунт"},
	"sign_out":                       {"Sign out", "Выйти"},
	"skip_to_content":                {"Skip to content", "К содержимому"},
	"language":                       {"Language", "Язык"},
	"appearance":                     {"Appearance", "Оформление"},
	"light":                          {"Light", "Светлая"},
	"dark":                           {"Dark", "Тёмная"},
	"system":                         {"System", "Системная"},
	"english":                        {"English", "Английский"},
	"russian":                        {"Russian", "Русский"},
	"brand_descriptor":               {"Integration intelligence", "Контроль интеграций"},
	"landing_eyebrow":                {"A clearer way to stay in sync", "Ясность в каждом обмене"},
	"landing_title":                  {"Every business event, accounted for.", "Каждое бизнес-событие под контролем."},
	"landing_body":                   {"Bring payments, customer records and notifications together with a clear trail from event to outcome.", "Объедините платежи, данные клиентов и уведомления. Прослеживайте путь от события до результата."},
	"landing_signal_one":             {"Connected systems", "Связанные системы"},
	"landing_signal_two":             {"Visible outcomes", "Понятный результат"},
	"landing_signal_three":           {"Controlled recovery", "Управляемое восстановление"},
	"landing_signin_title":           {"Enter your workspace", "Войдите в рабочую область"},
	"landing_signin_body":            {"Sign in to review connections, follow each run and resolve work that needs attention.", "Войдите, чтобы проверить подключения, отслеживать запуски и разбирать задачи, требующие внимания."},
	"landing_signin_button":          {"Continue to sign in", "Продолжить вход"},
	"landing_signin_note":            {"Secure account sign-in", "Защищённый вход в аккаунт"},
	"workspace_eyebrow":              {"Your environment", "Ваше пространство"},
	"workspace_title":                {"Choose a workspace", "Выберите рабочую область"},
	"workspace_body":                 {"Select where you want to work. Each workspace keeps its connections and activity separate.", "Выберите область для работы. Подключения и события каждой области изолированы."},
	"workspace_open":                 {"Open workspace", "Открыть область"},
	"workspace_empty_title":          {"Make room for your first workflow", "Создайте место для первого сценария"},
	"workspace_empty_body":           {"Create a workspace to organize connections, automation and execution history.", "Создайте область для подключений, автоматизации и истории запусков."},
	"workspace_create_title":         {"Create a workspace", "Создать рабочую область"},
	"workspace_name":                 {"Workspace name", "Название области"},
	"workspace_create_button":        {"Create workspace", "Создать область"},
	"workspace_next":                 {"Next page", "Следующая страница"},
	"workspace_signed_in":            {"Signed in as", "Вы вошли как"},
	"overview_title":                 {"Workspace overview", "Обзор рабочей области"},
	"overview_body":                  {"A live view of connections, automation and work waiting in this workspace. Only a fully succeeded run confirms every required effect.", "Подключения, автоматизация и ожидающие задачи в одной картине. Только полностью успешный запуск подтверждает все необходимые действия."},
	"backlog":                        {"Backlog", "Очередь"},
	"backlog_body":                   {"Actions pending, running, paused or waiting for retry.", "Ожидающие, выполняющиеся, приостановленные и повторные действия."},
	"successful_runs":                {"Successful runs", "Успешные запуски"},
	"last_sync":                      {"Last sync:", "Последняя синхронизация:"},
	"none_yet":                       {"None yet", "Пока нет"},
	"active_automations":             {"Active automations", "Активные сценарии"},
	"active_automations_body":        {"Subscription Billing Sync v1 revisions.", "Активные версии сценария синхронизации подписок."},
	"needs_action":                   {"Needs action", "Требует внимания"},
	"inspect_runs":                   {"Inspect runs", "Открыть запуски"},
	"getting_started":                {"Getting started", "Начало работы"},
	"recent_business_events":         {"Recent business events", "Недавние события"},
	"latest_webhooks":                {"Latest eight accepted webhook records", "Последние восемь принятых webhook-событий"},
	"event":                          {"Event", "Событие"},
	"type":                           {"Type", "Тип"},
	"received":                       {"Received", "Получено"},
	"no_events":                      {"No Stripe events have been received for this workspace yet. Finish setup, then send one Test Mode billing event.", "В эту область пока не поступали события Stripe. Завершите настройку и отправьте тестовое событие оплаты."},
	"recent_runs":                    {"Recent runs", "Недавние запуски"},
	"view_all_runs":                  {"View all runs", "Все запуски"},
	"run":                            {"Run", "Запуск"},
	"source":                         {"Source", "Источник"},
	"status":                         {"Status", "Статус"},
	"started":                        {"Started", "Начало"},
	"no_runs":                        {"No runs yet. A supported Stripe Test Mode event will appear here after the automation is active.", "Запусков пока нет. Поддерживаемое тестовое событие Stripe появится здесь после активации автоматизации."},
	"open_step":                      {"Open step", "Перейти к шагу"},
	"Connect Stripe Test Mode":       {"Connect Stripe Test Mode", "Подключить Stripe Test Mode"},
	"Connect HubSpot":                {"Connect HubSpot", "Подключить HubSpot"},
	"Connect Slack":                  {"Connect Slack", "Подключить Slack"},
	"Activate billing automation":    {"Activate billing automation", "Активировать сценарий оплаты"},
	"Observe first event":            {"Observe first event", "Проверить первое событие"},
	"Verify the provider account and permissions.":               {"Verify the provider account and permissions.", "Проверьте аккаунт и разрешения провайдера."},
	"Select verified connections and review the first revision.": {"Select verified connections and review the first revision.", "Выберите проверенные подключения и просмотрите первую версию сценария."},
	"Active revision is paused":                                  {"Active revision is paused", "Активная версия приостановлена"},
	"Send a supported Stripe Test Mode event after activation.":  {"Send a supported Stripe Test Mode event after activation.", "После активации отправьте поддерживаемое тестовое событие Stripe."},
	"At least one supported event has a persisted run.":          {"At least one supported event has a persisted run.", "Хотя бы одно поддерживаемое событие создало сохранённый запуск."},
	"Verified":                        {"Verified", "Проверено"},
	"verified_connection_fmt":         {"Verified %s connection %s", "Проверено подключение %s · %s"},
	"test":                            {"test", "тестовое"},
	"simulation":                      {"simulation", "симуляция"},
	"Unverified":                      {"Unverified", "Не проверено"},
	"Disconnected":                    {"Disconnected", "Отключено"},
	"Invalid":                         {"Invalid", "Некорректно"},
	"Degraded":                        {"Degraded", "Работает с ограничениями"},
	"Active revision":                 {"Active revision", "Активная версия"},
	"Connection needs attention:":     {"Connection needs attention:", "Подключение требует внимания:"},
	"waiting":                         {"Waiting", "Ожидает"},
	"complete":                        {"Complete", "Готово"},
	"blocked":                         {"Blocked", "Заблокировано"},
	"unknown_result_notice":           {"A Slack result is unknown. Check the actual channel before deciding whether to confirm or accept duplicate risk.", "Результат Slack неизвестен. Проверьте канал, прежде чем подтверждать сообщение или принимать риск дубля."},
	"review_unknown_runs":             {"Review unknown runs", "Открыть неопределённые запуски"},
	"coverage_gap":                    {"Reconciliation coverage gap", "Пробел в покрытии сверки"},
	"coverage_gap_body":               {"Some Stripe history may be missing.", "Часть истории Stripe может отсутствовать."},
	"review_coverage":                 {"Review coverage and safe next steps", "Проверить покрытие и дальнейшие шаги"},
	"outbox_age":                      {"Business outbox age:", "Возраст очереди исходящих задач:"},
	"outbox_age_body":                 {"Review worker and broker health in", "Проверьте состояние worker и брокера в разделе"},
	"operations":                      {"Operations", "Операции"},
	"provider_connect_title":          {"Connect provider", "Подключение провайдера"},
	"provider_connect_body":           {"Continue to review the account and requested permissions with the provider.", "Продолжите у провайдера, чтобы проверить аккаунт и запрошенные разрешения."},
	"provider_connect_button":         {"Review permissions", "Проверить разрешения"},
	"provider_connected_title":        {"Provider connected", "Провайдер подключён"},
	"provider_connected_body":         {"The connection has been verified.", "Подключение проверено."},
	"provider_return":                 {"Return to workspaces", "Вернуться к областям"},
	"role_owner":                      {"Owner", "Владелец"},
	"role_operator":                   {"Operator", "Оператор"},
	"role_viewer":                     {"Viewer", "Наблюдатель"},
	"Connection verified and stored.": {"Connection verified and stored.", "Подключение проверено и сохранено."},
	"Connection disconnected; its stored credentials were revoked.":                   {"Connection disconnected; its stored credentials were revoked.", "Подключение отключено; сохранённые учётные данные отозваны."},
	"Automation revision activated for new events.":                                   {"Automation revision activated for new events.", "Новая версия автоматизации активирована для будущих событий."},
	"Outbound effects paused. Accepted work remains durable.":                         {"Outbound effects paused. Accepted work remains durable.", "Внешние действия приостановлены. Принятая работа сохранена."},
	"Outbound effects resumed.":                                                       {"Outbound effects resumed.", "Внешние действия возобновлены."},
	"Operator decision recorded. Refresh to see the current action state.":            {"Operator decision recorded. Refresh to see the current action state.", "Решение оператора записано. Обновите страницу, чтобы увидеть состояние действия."},
	"Contact mapping saved. Retry only the eligible CRM action from this run.":        {"Contact mapping saved. Retry only the eligible CRM action from this run.", "Связь контакта сохранена. Повторите только допустимое CRM-действие этого запуска."},
	"Bounded historical reconciliation requested. Follow its persisted status below.": {"Bounded historical reconciliation requested. Follow its persisted status below.", "Запрошена ограниченная историческая сверка. Её сохранённый статус показан ниже."},
	"Sign-in status unavailable":                                                      {"Sign-in status unavailable", "Состояние входа недоступно"},
	"Workspaces unavailable":                                                          {"Workspaces unavailable", "Рабочие области недоступны"},
	"Invalid page cursor":                                                             {"Invalid page cursor", "Некорректный указатель страницы"},
	"Invalid form":                                                                    {"Invalid form", "Некорректная форма"},
	"Enter a workspace name (1–120 characters)":                                       {"Enter a workspace name (1–120 characters)", "Введите название области (1–120 символов)"},
	"Workspace creation failed":                                                       {"Workspace creation failed", "Не удалось создать рабочую область"},
	"dashboard_eyebrow":                                                               {"Operational picture", "Операционная картина"},
	"dashboard_metrics":                                                               {"Workspace metrics", "Показатели рабочей области"},
	"overall_setup":                                                                   {"Setup in progress", "Идёт настройка"},
	"overall_detail_setup":                                                            {"Complete the guided steps below", "Завершите шаги ниже"},
	"overall_healthy":                                                                 {"Operating normally", "Работает штатно"},
	"overall_detail_healthy":                                                          {"No flagged work in this view", "В этом обзоре нет отмеченных проблем"},
	"overall_attention":                                                               {"Review needed", "Нужна проверка"},
	"overall_detail_attention":                                                        {"Inspect the items below", "Проверьте задачи ниже"},
	"onboarding_eyebrow":                                                              {"Guided setup", "Пошаговая настройка"},
	"onboarding_body":                                                                 {"Connect each service, activate the workflow, then observe its first persisted run.", "Подключите сервисы, активируйте сценарий и проверьте первый сохранённый запуск."},
	"onboarding_progress":                                                             {"Setup progress", "Ход настройки"},
	"steps_complete":                                                                  {"steps complete", "шагов завершено"},
	"onboarding_status":                                                               {"Setup status", "Состояние настройки"},
	"activity_eyebrow":                                                                {"Execution history", "История выполнения"},
	"run_trend":                                                                       {"Runs over time", "Запуски по дням"},
	"last_14_days_utc":                                                                {"Last 14 days · UTC", "Последние 14 дней · UTC"},
	"runs_in_period":                                                                  {"in this period", "за этот период"},
	"peak_day":                                                                        {"Peak daily count", "Максимум за день"},
	"chart_tooltip_hint":                                                              {"Hover or focus a bar for its date and count.", "Наведите курсор или выберите столбец для даты и количества."},
	"date":                                                                            {"Date", "Дата"},
	"runs_count":                                                                      {"Runs", "Запуски"},
	"no_activity_title":                                                               {"Your first run will appear here", "Первый запуск появится здесь"},
	"no_activity_body":                                                                {"The last 14 UTC days contain no persisted runs. Connect providers and activate automation to start tracking activity.", "За последние 14 дней UTC нет сохранённых запусков. Подключите провайдеров и активируйте автоматизацию."},
	"review_connections":                                                              {"Review connections", "Проверить подключения"},
	"provider_layer":                                                                  {"Connected services", "Подключённые сервисы"},
	"provider_health":                                                                 {"Provider health", "Состояние провайдеров"},
	"manage_connections":                                                              {"Manage connections", "Управлять подключениями"},
	"event_layer":                                                                     {"Accepted input", "Принятые события"},
	"connection_verified":                                                             {"Verified", "Проверено"},
	"connection_attention":                                                            {"Needs attention", "Требует внимания"},
	"connection_disconnected":                                                         {"Not connected", "Не подключено"},
	"connections_intro":                                                               {"Confirm the exact external account before connecting. Only workspace owners can change credentials; stored secrets are never displayed.", "Перед подключением проверьте точный внешний аккаунт. Только владельцы могут менять учётные данные; сохранённые секреты не отображаются."},
	"No providers connected":                                                          {"No providers connected", "Провайдеры не подключены"},
	"connections_empty_body":                                                          {"Start with Stripe Test Mode, then connect the intended HubSpot portal and Slack workspace.", "Сначала подключите Stripe Test Mode, затем нужный портал HubSpot и область Slack."},
	"provider_intro_stripe":                                                           {"Billing event source", "Источник событий оплаты"},
	"provider_intro_hubspot":                                                          {"Customer records", "Данные клиентов"},
	"provider_intro_slack":                                                            {"Operational notifications", "Рабочие уведомления"},
	"verified_account_identity":                                                       {"Account identity", "Идентификатор аккаунта"},
	"environment":                                                                     {"Environment", "Среда"},
	"last_verified":                                                                   {"Verified at", "Проверено"},
	"last_checked":                                                                    {"Last checked", "Последняя проверка"},
	"credential_health":                                                               {"Credential", "Учётные данные"},
	"not_recorded":                                                                    {"Not recorded", "Не записано"},
	"not_checked":                                                                     {"Not checked", "Не проверено"},
	"local_simulator_fixture":                                                         {"Local simulator fixture", "Локальный симулятор"},
	"restricted_test_key":                                                             {"Restricted Test Mode key", "Ограниченный ключ Test Mode"},
	"credential_revoked":                                                              {"Stored credentials revoked", "Сохранённые учётные данные отозваны"},
	"grant_status_unavailable":                                                        {"OAuth grant status unavailable", "Состояние OAuth-разрешений недоступно"},
	"expires":                                                                         {"expires", "истекает"},
	"required_access":                                                                 {"Required access", "Необходимые разрешения"},
	"required_access_stripe":                                                          {"Core sync: Accounts, Customers, Subscriptions and Invoices Read. Historical discovery also needs Events Read.", "Для основной синхронизации: чтение Accounts, Customers, Subscriptions и Invoices. Для исторического поиска также нужно Events Read."},
	"required_access_hubspot":                                                         {"oauth, crm.objects.contacts.read, crm.objects.contacts.write", "oauth, crm.objects.contacts.read, crm.objects.contacts.write"},
	"required_access_slack":                                                           {"Bot chat:write", "Право бота chat:write"},
	"connection_remedy":                                                               {"This connection is not verified or its authorization needs attention. Review identity and reconnect the same account if appropriate.", "Подключение не проверено или авторизация требует внимания. Проверьте аккаунт и при необходимости переподключите его."},
	"replace_credentials":                                                             {"Replace Test Mode credentials", "Заменить тестовые учётные данные"},
	"reconnect_same_account":                                                          {"Reconnect same account", "Переподключить тот же аккаунт"},
	"disconnect":                                                                      {"Disconnect", "Отключить"},
	"disconnect_warning":                                                              {"Stored credentials will be revoked. Accepted work stays durable; review dependent automation first.", "Сохранённые учётные данные будут отозваны. Принятая работа сохранится; сначала проверьте зависимую автоматизацию."},
	"confirm_disconnect":                                                              {"Confirm disconnect", "Подтвердить отключение"},
	"provider_not_connected":                                                          {"No active connection yet", "Активного подключения пока нет"},
	"Set up Stripe":                                                                   {"Set up Stripe", "Настроить Stripe"},
	"add_another_stripe":                                                              {"Add another Stripe Test Mode account", "Добавить ещё один аккаунт Stripe Test Mode"},
	"add_another_hubspot":                                                             {"Add another HubSpot portal", "Добавить ещё один портал HubSpot"},
	"add_another_slack":                                                               {"Add another Slack workspace", "Добавить ещё одну область Slack"},
	"setup_intro_stripe":                                                              {"Connect one restricted Stripe Test Mode key and a signed snapshot webhook destination.", "Подключите ограниченный ключ Stripe Test Mode и подписанный webhook для snapshot-событий."},
	"setup_intro_hubspot":                                                             {"Enter the intended test portal ID before opening provider authorization.", "Введите ID нужного тестового портала перед авторизацией у провайдера."},
	"setup_intro_slack":                                                               {"Enter the intended workspace ID before authorizing the bot.", "Введите ID нужной рабочей области перед авторизацией бота."},
	"expected_hubspot_portal":                                                         {"Expected HubSpot portal ID", "Ожидаемый ID портала HubSpot"},
	"expected_slack_workspace":                                                        {"Expected Slack workspace ID", "Ожидаемый ID области Slack"},
	"identity_confirmation_help":                                                      {"The returned identity must match this value exactly.", "Возвращённый идентификатор должен точно совпадать с этим значением."},
	"setup_help":                                                                      {"Setup details and permissions", "Детали настройки и разрешения"},
	"setup_help_stripe":                                                               {"Use a test-only key and destination. LenoirDesk verifies the account and required read access before storing encrypted credentials.", "Используйте только тестовый ключ и destination. Перед сохранением зашифрованных данных LenoirDesk проверит аккаунт и права чтения."},
	"setup_help_hubspot":                                                              {"Install the OAuth app only in the intended test portal. No extra scope is requested by this screen.", "Установите OAuth-приложение только в нужный тестовый портал. Экран не запрашивает дополнительных прав."},
	"setup_help_slack":                                                                {"Install the bot in the intended test workspace and invite it to the selected private channel.", "Установите бота в нужную тестовую область и добавьте его в выбранный приватный канал."},
	"connection_owner_only":                                                           {"Connection changes are available to workspace owners.", "Менять подключения могут владельцы рабочей области."},
	"ready":                                                                           {"Ready", "Готово"},
	"refreshing":                                                                      {"Refreshing", "Обновляется"},
	"Succeeded":                                                                       {"Succeeded", "Успешно"},
	"Planned":                                                                         {"Planned", "Запланировано"},
	"Executing":                                                                       {"In progress", "В работе"},
	"Waiting to retry":                                                                {"Waiting to retry", "Ожидает повтора"},
	"Connection needs attention":                                                      {"Connection needs attention", "Подключение требует внимания"},
	"Configuration blocked":                                                           {"Configuration blocked", "Настройка заблокирована"},
	"Needs attention":                                                                 {"Needs attention", "Требует внимания"},
	"Outcome unknown":                                                                 {"Outcome unknown", "Результат неизвестен"},
	"Dead letter":                                                                     {"Dead letter", "Ошибочная задача"},
	"Paused":                                                                          {"Paused", "Приостановлено"},
	"Active":                                                                          {"Active", "Активно"},
	"inactive":                                                                        {"Inactive", "Неактивно"},
	"skipped":                                                                         {"Skipped", "Пропущено"},
	"canceled":                                                                        {"Canceled", "Отменено"},
	"superseded":                                                                      {"Superseded", "Заменено"},
	"All":                                                                             {"All", "Все"},
	"Pending / running":                                                               {"Pending / running", "Ожидают / выполняются"},
	"Retry waiting":                                                                   {"Retry waiting", "Ожидают повтора"},
	"Blocked":                                                                         {"Blocked", "Заблокированы"},
	"Unknown":                                                                         {"Unknown", "Неизвестный результат"},
	"Dead / DLQ":                                                                      {"Stopped retries (DLQ)", "Остановленные повторы (DLQ)"},
	"automation_eyebrow":                                                              {"Bounded workflow", "Ограниченный сценарий"},
	"automation_title":                                                                {"Subscription billing sync", "Синхронизация подписок"},
	"automation_intro":                                                                {"One supported workflow turns Stripe Test Mode billing events into managed HubSpot Contact updates, then sends eligible paid-invoice notifications to Slack.", "Один поддерживаемый сценарий переносит события оплаты Stripe Test Mode в управляемые поля контактов HubSpot, затем отправляет допустимые уведомления об оплаченных счетах в Slack."},
	"automation_not_active":                                                           {"Not active", "Не активировано"},
	"automation_revision":                                                             {"Revision", "Версия"},
	"automation_activated":                                                            {"Activated", "Активирована"},
	"automation_no_revision":                                                          {"No revision activated", "Нет активированной версии"},
	"automation_pause":                                                                {"Pause outbound effects", "Приостановить внешние действия"},
	"automation_resume":                                                               {"Resume outbound effects", "Возобновить внешние действия"},
	"automation_configure":                                                            {"Configure workflow", "Настроить сценарий"},
	"automation_flow_label":                                                           {"Stripe to HubSpot to Slack workflow", "Сценарий Stripe → HubSpot → Slack"},
	"automation_source":                                                               {"Source", "Источник"},
	"automation_projection":                                                           {"Projection", "Синхронизация"},
	"automation_notification":                                                         {"Notification", "Уведомление"},
	"automation_stripe_purpose":                                                       {"Accept signed subscription and invoice events in Test Mode.", "Принимать подписанные события подписок и счетов в тестовом режиме."},
	"automation_hubspot_purpose":                                                      {"Update only managed Contact fields after identity checks.", "Обновлять только управляемые поля контакта после проверки личности."},
	"automation_slack_purpose":                                                        {"Notify after CRM confirmation when an invoice is eligible.", "Уведомлять после подтверждения CRM, если счёт подходит."},
	"automation_verified_available":                                                   {"Verified Test Mode connection available", "Есть проверенное тестовое подключение"},
	"automation_connection_needed":                                                    {"Verified Test Mode connection needed", "Нужно проверенное тестовое подключение"},
	"automation_configuration":                                                        {"Configuration", "Настройка"},
	"automation_new_revision":                                                         {"Prepare a new revision", "Подготовить новую версию"},
	"automation_first_revision":                                                       {"Prepare the first revision", "Подготовить первую версию"},
	"automation_config_intro":                                                         {"Select verified accounts and define the bounded business rules. Review every value before activation.", "Выберите проверенные аккаунты и задайте правила сценария. Перед активацией проверьте каждое значение."},
	"automation_proposed_revision":                                                    {"Proposed revision", "Предполагаемая версия"},
	"automation_connected_accounts":                                                   {"Verified connections", "Проверенные подключения"},
	"automation_verified_help":                                                        {"Only active, verified Test Mode connections can be selected.", "Доступны только активные, проверенные тестовые подключения."},
	"automation_manage_connections":                                                   {"Manage connections", "Управлять подключениями"},
	"automation_stripe_account":                                                       {"Stripe account", "Аккаунт Stripe"},
	"automation_hubspot_portal":                                                       {"HubSpot portal", "Портал HubSpot"},
	"automation_slack_workspace":                                                      {"Slack workspace", "Рабочая область Slack"},
	"automation_choose_stripe":                                                        {"Choose verified Stripe", "Выберите проверенный Stripe"},
	"automation_choose_hubspot":                                                       {"Choose verified HubSpot", "Выберите проверенный HubSpot"},
	"automation_choose_slack":                                                         {"Choose verified Slack", "Выберите проверенный Slack"},
	"automation_stripe_help":                                                          {"Exact account identity is checked at activation.", "При активации проверяется точный ID аккаунта."},
	"automation_hubspot_help":                                                         {"Contacts require the approved read and write grant.", "Для контактов нужны согласованные права чтения и записи."},
	"automation_slack_help":                                                           {"The bot needs access to the destination channel.", "Боту нужен доступ к целевому каналу."},
	"automation_business_rules":                                                       {"Business rules", "Бизнес-правила"},
	"automation_prices":                                                               {"Tracked Stripe Price IDs", "Отслеживаемые Price ID Stripe"},
	"automation_prices_help":                                                          {"Enter 1–20 distinct Test Mode Price IDs, one per line or separated by commas. Edit or remove a line to change the list. The restricted key cannot browse the Price catalog.", "Введите от 1 до 20 уникальных тестовых Price ID: по одному в строке или через запятую. Для изменения отредактируйте или удалите строку. Ограниченный ключ не открывает каталог цен."},
	"automation_channel":                                                              {"Slack destination channel ID", "ID целевого канала Slack"},
	"automation_channel_help":                                                         {"Copy the channel ID from Slack channel details. For a private channel, invite the bot first; no channel-listing permission is requested.", "Скопируйте ID в сведениях о канале Slack. Для приватного канала сначала добавьте бота; право на просмотр списка каналов не запрашивается."},
	"automation_policy":                                                               {"Contact policy", "Политика контактов"},
	"policy_explicit_only":                                                            {"Explicit operator link only", "Только явная связь оператором"},
	"policy_unique_upsert":                                                            {"Unique customer-key upsert for unlinked customers", "Upsert по уникальному ключу клиента для несвязанных клиентов"},
	"automation_policy_help":                                                          {"Existing Contacts are never matched by email alone. Identity conflicts stop for review.", "Существующие контакты никогда не сопоставляются только по email. Конфликты личности требуют проверки."},
	"automation_review_note":                                                          {"Review opens a confirmation screen. No revision activates while editing this form.", "Откроется экран подтверждения. При редактировании формы версия не активируется."},
	"automation_review_button":                                                        {"Review configuration", "Проверить настройку"},
	"automation_viewer_notice":                                                        {"Viewer access is read-only. Ask an owner or operator to prepare a revision.", "Наблюдатель может только просматривать. Попросите владельца или оператора подготовить версию."},
	"automation_revision_history":                                                     {"Revision history", "История версий"},
	"automation_recent_revisions":                                                     {"recent revisions", "последних версий"},
	"automation_contract":                                                             {"Operational contract", "Правила выполнения"},
	"automation_safe_by_design":                                                       {"What this workflow protects", "Что защищает этот сценарий"},
	"automation_warning_multiple":                                                     {"Multiple matching subscriptions stop for operator review.", "Несколько подходящих подписок останавливают запуск для проверки оператором."},
	"automation_warning_new_events":                                                   {"A new revision applies only to events accepted after activation.", "Новая версия применяется только к событиям, принятым после активации."},
	"automation_warning_historical":                                                   {"Historical discovery updates CRM but remains Slack-silent.", "Историческое обнаружение обновляет CRM, но не отправляет Slack-уведомления."},
	"automation_warning_identity":                                                     {"Provider identity, Test Mode and permissions must remain verified.", "Личность провайдера, тестовый режим и разрешения должны оставаться проверенными."},
	"automation_managed_fields":                                                       {"Managed HubSpot fields", "Управляемые поля HubSpot"},
	"automation_fields_help":                                                          {"These Contact properties may be overwritten on the next projection.", "Следующая синхронизация может перезаписать эти свойства контакта."},
	"automation_review_eyebrow":                                                       {"Activation review", "Проверка перед активацией"},
	"automation_review_title":                                                         {"Review automation", "Проверьте автоматизацию"},
	"automation_review_intro":                                                         {"Confirm accounts, destination and policy before creating an immutable revision. Nothing is activated until you confirm below.", "Подтвердите аккаунты, канал и политику перед созданием неизменяемой версии. Активация произойдёт только после подтверждения ниже."},
	"automation_review_summary":                                                       {"Configuration summary", "Сводка настройки"},
	"automation_test_mode":                                                            {"Test Mode", "Тестовый режим"},
	"automation_revision_provisional":                                                 {"Final number is assigned at activation", "Итоговый номер назначается при активации"},
	"automation_activate":                                                             {"Activate this revision", "Активировать эту версию"},
	"automation_edit_setup":                                                           {"Edit setup", "Изменить настройку"},
	"automation_expected_behavior":                                                    {"Expected workflow", "Ожидаемое выполнение"},
	"automation_exceptions":                                                           {"Exceptions", "Исключения"},
	"automation_warning_unknown":                                                      {"An unknown Slack outcome needs observation before retrying; duplicates are possible.", "Неизвестный результат Slack требует проверки перед повтором; возможны дубли."},
	"automation_activation_failed":                                                    {"Activation was not saved. Connections or configuration may have changed; review current values before trying again.", "Активация не сохранена. Подключения или настройки могли измениться; проверьте текущие значения перед повтором."},
	"automation_pause_failed":                                                         {"Pause state was not changed. Reload and review the active revision before trying again.", "Состояние паузы не изменено. Обновите страницу и проверьте активную версию перед повтором."},
	"automation_invalid_form":                                                         {"Review the highlighted fields, then try again.", "Проверьте отмеченные поля и повторите."},
	"automation_invalid_connections":                                                  {"Choose three active, verified Test Mode connections in this workspace.", "Выберите три активных, проверенных тестовых подключения в этой рабочей области."},
	"automation_connection_required":                                                  {"Choose a verified connection.", "Выберите проверенное подключение."},
	"automation_connection_unavailable":                                               {"This connection is not currently verified for this workspace.", "Это подключение сейчас не подтверждено для этой рабочей области."},
	"automation_account_unavailable":                                                  {"Connection unavailable", "Подключение недоступно"},
	"automation_prices_count_error":                                                   {"Enter between 1 and 20 Price IDs.", "Введите от 1 до 20 Price ID."},
	"automation_prices_format_error":                                                  {"Each value must be a valid price_ ID.", "Каждое значение должно быть корректным ID вида price_."},
	"automation_prices_duplicate_error":                                               {"Remove duplicate Price IDs.", "Удалите повторяющиеся Price ID."},
	"automation_channel_error":                                                        {"Enter a valid Slack channel ID beginning with C or G.", "Введите корректный ID канала Slack, начинающийся с C или G."},
	"automation_policy_error":                                                         {"Choose a supported Contact policy.", "Выберите поддерживаемую политику контактов."},
	"runs_eyebrow":                                                                    {"Operations", "Операции"},
	"runs_title":                                                                      {"Execution history", "История выполнения"},
	"runs_intro":                                                                      {"Newest persisted runs first. A run succeeds only after every required action is confirmed.", "Сначала новые сохранённые запуски. Запуск успешен только после подтверждения всех обязательных действий."},
	"runs_total":                                                                      {"Total runs", "Всего запусков"},
	"runs_summary":                                                                    {"Run status summary", "Сводка статусов запусков"},
	"runs_activity":                                                                   {"Activity", "Активность"},
	"runs_filter_help":                                                                {"Filter by durable status. Open a run for action history and safe recovery.", "Фильтруйте по сохранённому статусу. Откройте запуск для истории действий и безопасного восстановления."},
	"runs_page_size":                                                                  {"Up to 25 runs per page", "До 25 запусков на странице"},
	"runs_status_filters":                                                             {"Run status filters", "Фильтры статусов запусков"},
	"runs_business_identity":                                                          {"Business reference", "Бизнес-идентификатор"},
	"runs_customer":                                                                   {"Customer", "Клиент"},
	"runs_invoice":                                                                    {"Invoice", "Счёт"},
	"runs_not_recorded":                                                               {"Not recorded", "Не записано"},
	"runs_attempts":                                                                   {"Attempts:", "Попыток:"},
	"runs_empty_title":                                                                {"No runs yet", "Запусков пока нет"},
	"runs_empty_body":                                                                 {"After activation, a supported Stripe Test Mode event will appear here with its persisted action states.", "После активации здесь появится поддерживаемое событие Stripe Test Mode с сохранёнными состояниями действий."},
	"runs_open_automation":                                                            {"Open automation", "Открыть автоматизацию"},
	"runs_filtered_empty_title":                                                       {"No matching runs", "Нет запусков по фильтру"},
	"runs_filtered_empty_body":                                                        {"No persisted run has this status. Choose another filter to continue.", "Нет сохранённых запусков с этим статусом. Выберите другой фильтр."},
	"runs_clear_filter":                                                               {"Show all runs", "Показать все запуски"},
	"runs_pagination":                                                                 {"Run history pagination", "Навигация по истории запусков"},
	"runs_newest_first":                                                               {"Newest first · UTC", "Сначала новые · UTC"},
	"runs_older":                                                                      {"Older runs", "Более ранние запуски"},
	"run_step_complete":                                                               {"All required actions confirmed", "Все обязательные действия подтверждены"},
	"run_step_crm":                                                                    {"HubSpot Contact", "Контакт HubSpot"},
	"run_step_slack":                                                                  {"Slack notification", "Уведомление Slack"},
	"run_step_planning":                                                               {"Planning", "Планирование"},
	"run_help_succeeded":                                                              {"Provider actions confirmed", "Действия провайдеров подтверждены"},
	"run_help_pending":                                                                {"Waiting for work", "Ожидает обработки"},
	"run_help_running":                                                                {"Execution is pending or underway", "Действие ожидает выполнения или выполняется"},
	"run_help_retry_wait":                                                             {"Automatic retry scheduled", "Автоматический повтор запланирован"},
	"run_help_blocked_auth":                                                           {"Reconnect provider before recovery", "Переподключите провайдера перед восстановлением"},
	"run_help_blocked_config":                                                         {"Review configuration before recovery", "Проверьте настройку перед восстановлением"},
	"run_help_needs_attention":                                                        {"Operator review required", "Нужна проверка оператора"},
	"run_help_unknown":                                                                {"Slack may have sent; inspect channel before deciding", "Slack мог отправить сообщение; проверьте канал"},
	"run_help_dead":                                                                   {"Automatic retries stopped; inspect details", "Автоповторы остановлены; откройте детали"},
	"run_help_paused":                                                                 {"Outbound effects paused", "Внешние действия приостановлены"},
	"run_help_skipped":                                                                {"Action intentionally skipped", "Действие намеренно пропущено"},
	"run_help_canceled":                                                               {"Run canceled", "Запуск отменён"},
	"run_help_superseded":                                                             {"Replaced by newer work", "Заменено более новой задачей"},
	"run_help_other":                                                                  {"Open run for details", "Откройте запуск для деталей"},
	"run_inspector_eyebrow":                                                           {"Business execution", "Бизнес-исполнение"},
	"run_inspector_title":                                                             {"Run details", "Детали запуска"},
	"run_inspector_intro":                                                             {"Follow the recorded event, required actions and safe next steps.", "Проследите событие, действия и безопасные следующие шаги."},
	"run_back_to_history":                                                             {"Execution history", "История запусков"},
	"restore_blocked_title":                                                           {"Outbound execution is blocked.", "Исходящие действия заблокированы."},
	"restore_blocked_body":                                                            {"A restored environment awaits the established witness and operator clearance.", "После восстановления нужны штатная проверка и решение оператора."},
	"run_summary_eyebrow":                                                             {"Business summary", "Итог события"},
	"run_ref":                                                                         {"Run ID", "ID запуска"},
	"run_accepted":                                                                    {"Event recorded", "Событие сохранено"},
	"run_planned":                                                                     {"Run planned", "Запуск запланирован"},
	"run_finished":                                                                    {"Finished", "Завершён"},
	"run_still_open":                                                                  {"Still open", "Ещё выполняется"},
	"run_revision":                                                                    {"Automation revision", "Версия сценария"},
	"run_test_mode":                                                                   {"Test Mode", "Тестовый режим"},
	"run_source":                                                                      {"Event source", "Источник события"},
	"run_manual_attention":                                                            {"Manual attention required", "Требуется участие оператора"},
	"run_journey_eyebrow":                                                             {"Execution journey", "Путь выполнения"},
	"run_journey_title":                                                               {"From payment to outcome", "От платежа до результата"},
	"run_journey_hint":                                                                {"Each state reflects a persisted action.", "Каждое состояние основано на сохранённом действии."},
	"run_source_stage":                                                                {"Source", "Источник"},
	"run_crm_stage":                                                                   {"CRM", "CRM"},
	"run_notify_stage":                                                                {"Notification", "Уведомление"},
	"run_context_eyebrow":                                                             {"Source and identity", "Источник и идентификаторы"},
	"run_context_title":                                                               {"Business context", "Контекст события"},
	"run_stripe_event":                                                                {"Stripe event", "Событие Stripe"},
	"run_customer":                                                                    {"Stripe Customer", "Клиент Stripe"},
	"run_invoice":                                                                     {"Stripe Invoice", "Счёт Stripe"},
	"run_no_invoice":                                                                  {"No invoice action", "Действие со счётом не требуется"},
	"run_managed_key":                                                                 {"Managed Contact key", "Управляемый ключ контакта"},
	"run_revision_id":                                                                 {"Immutable revision ID", "ID неизменяемой версии"},
	"run_timeline_eyebrow":                                                            {"Recorded sequence", "Сохранённая последовательность"},
	"run_timeline_title":                                                              {"Timeline", "Хронология"},
	"run_actions_eyebrow":                                                             {"Provider effects", "Действия провайдеров"},
	"run_actions_title":                                                               {"Action details and recovery", "Детали действий и восстановление"},
	"run_action_id":                                                                   {"Action ID", "ID действия"},
	"run_attempts":                                                                    {"Attempts", "Попытки"},
	"run_next_retry":                                                                  {"Next automatic retry", "Следующий автоматический повтор"},
	"run_first_failure":                                                               {"First failure", "Первый сбой"},
	"run_confirmed_contact":                                                           {"Confirmed Contact", "Подтверждённый контакт"},
	"run_confirmed_message":                                                           {"Confirmed Slack message", "Подтверждённое сообщение Slack"},
	"run_crm_no_write_explanation":                                                    {"A provider read confirmed current CRM state; no HubSpot write was needed.", "Чтение подтвердило актуальное состояние CRM; запись в HubSpot не потребовалась."},
	"run_depends_on_crm":                                                              {"Depends on CRM action", "Зависит от действия CRM"},
	"run_review_mapping":                                                              {"Review Contact mapping", "Проверить связь контакта"},
	"run_retry_action":                                                                {"Retry this action", "Повторить это действие"},
	"run_retry_explanation":                                                           {"Verify the current provider state first. The server checks the current generation and records your reason.", "Сначала проверьте состояние провайдера. Сервер проверит версию действия и сохранит причину."},
	"run_retry_reason":                                                                {"Why is this action safe to retry?", "Почему действие безопасно повторить?"},
	"run_confirm_retry":                                                               {"Confirm retry", "Подтвердить повтор"},
	"run_unknown_warning_title":                                                       {"Outcome unknown.", "Результат неизвестен."},
	"run_unknown_warning":                                                             {"The message may already have been delivered. Retrying could send another notification.", "Сообщение уже могло быть доставлено. Повтор может отправить его ещё раз."},
	"run_confirm_delivery":                                                            {"Confirm observed delivery", "Подтвердить найденную доставку"},
	"run_observation_reason":                                                          {"Observation and audit reason", "Наблюдение и причина для аудита"},
	"run_observed_channel":                                                            {"Observed Slack channel ID", "ID проверенного канала Slack"},
	"run_observed_ts":                                                                 {"Observed Slack message timestamp", "Время проверенного сообщения Slack"},
	"run_confirm_existing_message":                                                    {"Confirm existing message", "Подтвердить существующее сообщение"},
	"run_leave_unknown":                                                               {"Leave unresolved", "Оставить без решения"},
	"run_risk_retry":                                                                  {"Risk-acknowledged retry", "Повтор с признанием риска"},
	"run_risk_retry_warning":                                                          {"Use only after checking the channel. This may create a duplicate message.", "Только после проверки канала. Возможен дубль сообщения."},
	"run_ack_duplicate":                                                               {"I accept the risk of a duplicate Slack notification.", "Я принимаю риск повторного уведомления Slack."},
	"run_attempt_details":                                                             {"Attempts and provider calls", "Попытки и обращения к провайдерам"},
	"run_attempt":                                                                     {"Attempt", "Попытка"},
	"run_external_gate":                                                               {"External request started", "Внешний запрос начат"},
	"run_request_ref":                                                                 {"Request reference", "Ссылка на запрос"},
	"run_no_attempts":                                                                 {"No execution attempt recorded yet.", "Попытки выполнения пока не зарегистрированы."},
	"run_operator_decisions":                                                          {"Operator decisions", "Решения оператора"},
	"run_no_decisions":                                                                {"No operator decision recorded.", "Решений оператора пока нет."},
	"run_no_actions":                                                                  {"Actions are not planned yet. Refresh after the planner processes this event.", "Действия ещё не запланированы. Обновите страницу позже."},
	"run_audit_title":                                                                 {"Audit history", "История аудита"},
	"run_no_audit":                                                                    {"No run-specific audit entries yet.", "Записей аудита для запуска пока нет."},
	"run_viewer_note":                                                                 {"Viewer access is read-only; recovery controls are hidden.", "Для наблюдателя доступен только просмотр."},
	"mapping_eyebrow":                                                                 {"Verified identity", "Проверенная идентичность"},
	"mapping_title":                                                                   {"Review Contact mapping", "Проверка связи контакта"},
	"mapping_intro":                                                                   {"Bind an existing HubSpot Contact only after checking identity. Never match or merge by email alone.", "Свяжите существующий контакт HubSpot после проверки. Не сопоставляйте по одному email."},
	"mapping_identity":                                                                {"Business identity", "Бизнес-идентичность"},
	"mapping_hubspot_connection":                                                      {"HubSpot connection", "Подключение HubSpot"},
	"mapping_current_link":                                                            {"Current link", "Текущая связь"},
	"mapping_no_link":                                                                 {"No persisted mapping", "Сохранённой связи нет"},
	"mapping_archive_warning":                                                         {"An archived or merged Contact may be hidden. Investigate before binding if identity is uncertain.", "Архивный или объединённый контакт может быть скрыт. При сомнениях сначала проверьте идентичность."},
	"mapping_inspect":                                                                 {"Inspect proposed Contact", "Проверить предложенный контакт"},
	"mapping_contact_id":                                                              {"Numeric HubSpot Contact ID", "Числовой ID контакта HubSpot"},
	"mapping_read_contact":                                                            {"Read Contact", "Прочитать контакт"},
	"mapping_contact":                                                                 {"Contact", "Контакт"},
	"mapping_identity_status":                                                         {"Identity", "Идентичность"},
	"mapping_status_matching":                                                         {"Matching customer key", "Ключ клиента совпадает"},
	"mapping_status_unclaimed":                                                        {"Unclaimed Contact", "Контакт без закреплённого ключа"},
	"mapping_status_conflict":                                                         {"Conflicting customer key", "Конфликт ключа клиента"},
	"mapping_key_unset":                                                               {"Not set", "Не задан"},
	"mapping_bind":                                                                    {"Bind and audit", "Связать с записью в аудите"},
	"mapping_reason":                                                                  {"Why is this the correct Contact?", "Почему это правильный контакт?"},
	"mapping_bind_button":                                                             {"Bind Contact", "Связать контакт"},
	"stage_not_planned":                                                               {"Not planned", "Не запланировано"},
	"stage_not_applicable":                                                            {"Not applicable", "Не требуется"},
	"stage_suppressed":                                                                {"Suppressed by policy", "Подавлено политикой"},
	"stage_source_recorded":                                                           {"The event is durably recorded.", "Событие надёжно сохранено."},
	"stage_crm_unplanned":                                                             {"CRM work has not been planned yet.", "Работа с CRM ещё не запланирована."},
	"stage_slack_unplanned":                                                           {"Notification work has not been planned yet.", "Уведомление ещё не запланировано."},
	"stage_slack_not_applicable":                                                      {"This event does not require a Slack notification.", "Для события уведомление Slack не требуется."},
	"stage_slack_suppressed":                                                          {"Historical discovery updates CRM without retroactive Slack.", "Историческое обнаружение обновляет CRM без запоздалого Slack."},
	"stage_crm_no_write":                                                              {"CRM state was confirmed without an external write.", "Состояние CRM подтверждено без внешней записи."},
	"source_webhook":                                                                  {"Signed webhook", "Подписанный webhook"},
	"source_event_discovery":                                                          {"Stripe event discovery", "Обнаружение события Stripe"},
	"source_known_state":                                                              {"Known-customer scan", "Проверка известного клиента"},
	"source_historical":                                                               {"Historical scan", "Историческая проверка"},
	"timeline_event_recorded":                                                         {"Event recorded", "Событие сохранено"},
	"timeline_run_planned":                                                            {"Run planned", "Запуск запланирован"},
	"timeline_attempt":                                                                {"Provider attempt started", "Попытка обращения к провайдеру"},
	"timeline_provider_call":                                                          {"Provider call recorded", "Обращение к провайдеру сохранено"},
	"timeline_attempt_result":                                                         {"Attempt result recorded", "Результат попытки сохранён"},
	"timeline_operator_decision":                                                      {"Operator decision recorded", "Решение оператора сохранено"},
	"timeline_run_result":                                                             {"Run outcome recorded", "Результат запуска сохранён"},
	"run_no_error":                                                                    {"No recorded error.", "Ошибка не зарегистрирована."},
	"run_error_identity":                                                              {"Contact identity requires review.", "Требуется проверка идентичности контакта."},
	"run_error_auth":                                                                  {"Reconnect the provider before recovery.", "Переподключите провайдера перед восстановлением."},
	"run_error_quota":                                                                 {"Provider rate limit; retry may be scheduled.", "Ограничение частоты; может быть запланирован повтор."},
	"run_error_provider":                                                              {"Provider temporarily unavailable.", "Провайдер временно недоступен."},
	"run_error_unknown_slack":                                                         {"Slack may have accepted this message; inspect before retrying.", "Slack мог принять сообщение; проверьте перед повтором."},
	"run_error_generic":                                                               {"Provider action failed; use the request reference for support.", "Действие завершилось ошибкой; используйте ссылку на запрос."},
	"run_error_code":                                                                  {"Safe error code:", "Безопасный код ошибки:"},
	"reconcile_eyebrow":                                                               {"Operational command", "Операционный контроль"},
	"reconcile_title":                                                                 {"Reconciliation and operations", "Сверка и операции"},
	"reconcile_intro":                                                                 {"See verified coverage, missing history and work requiring attention. Request bounded Test Mode scans when authorized.", "Проверяйте покрытие, пропуски и задачи, требующие внимания. Запрашивайте ограниченные проверки тестовых событий."},
	"reconcile_health_eyebrow":                                                        {"Workspace health", "Состояние области"},
	"reconcile_last_complete":                                                         {"Last completed scan", "Последняя завершённая проверка"},
	"reconcile_never_verified":                                                        {"Not yet verified", "Ещё не подтверждено"},
	"reconcile_gap_count":                                                             {"Coverage gaps", "Пробелы покрытия"},
	"reconcile_overdue_age":                                                           {"Overdue retry age", "Возраст просроченного повтора"},
	"reconcile_coverage_eyebrow":                                                      {"Synchronization coverage", "Покрытие синхронизации"},
	"reconcile_coverage_title":                                                        {"Coverage checkpoints", "Контрольные точки покрытия"},
	"reconcile_gap":                                                                   {"Gap", "Пробел"},
	"reconcile_checkpoint_recorded":                                                   {"Checkpoint recorded", "Точка сохранена"},
	"reconcile_not_complete":                                                          {"Not completed", "Не завершено"},
	"reconcile_window":                                                                {"Window", "Период"},
	"reconcile_scanned_discovered":                                                    {"Scanned / discovered", "Проверено / найдено"},
	"reconcile_gap_reason":                                                            {"Gap reason", "Причина пробела"},
	"reconcile_gap_not_proven":                                                        {"This period has not been proven complete.", "Полнота этого периода не подтверждена."},
	"reconcile_no_history_title":                                                      {"No reconciliation history yet", "Истории сверки пока нет"},
	"reconcile_no_history_body":                                                       {"No checkpoint exists. Empty history is not verified coverage.", "Контрольной точки нет. Пустая история не означает подтверждённое покрытие."},
	"reconcile_coverage_limit":                                                        {"Stripe Events history is limited. A saved checkpoint alone does not prove all past events were recovered.", "История Stripe Events ограничена. Сохранённая точка сама по себе не доказывает восстановление всей истории."},
	"reconcile_processing_eyebrow":                                                    {"Processing health", "Состояние обработки"},
	"reconcile_processing_title":                                                      {"Pending work and exceptions", "Ожидающие задачи и исключения"},
	"reconcile_unknown_actions":                                                       {"Unknown outcomes", "Неизвестные результаты"},
	"reconcile_dead_actions":                                                          {"Actions with stopped retries (DLQ)", "Действия с остановленными повторами (DLQ)"},
	"reconcile_oldest":                                                                {"Oldest", "Старейшее"},
	"reconcile_retry_note":                                                            {"Only overdue retries are counted.", "Учитываются только просроченные повторы."},
	"reconcile_attention_title":                                                       {"Operator review needed", "Нужна проверка оператора"},
	"reconcile_attention_body":                                                        {"Inspect the affected runs before taking a recovery action.", "Проверьте затронутые запуски перед восстановлением."},
	"reconcile_open_runs":                                                             {"Open runs needing attention", "Открыть запуски с проблемами"},
	"reconcile_gap_attention_title":                                                   {"Coverage needs investigation", "Нужно проверить покрытие"},
	"reconcile_scan_eyebrow":                                                          {"Controlled scan", "Контролируемая проверка"},
	"reconcile_scan_title":                                                            {"Check earlier billing events", "Проверить прежние события оплаты"},
	"reconcile_scan_intro":                                                            {"Review account, UTC window and maximum work before submitting.", "Перед отправкой проверьте аккаунт, период UTC и лимит."},
	"reconcile_no_retro_slack":                                                        {"Historical reconciliation updates supported CRM projections but does not retroactively send Slack notifications.", "Историческая сверка обновляет поддерживаемые проекции CRM, но не отправляет запоздалые уведомления Slack."},
	"reconcile_choose_connection":                                                     {"Choose verified connection", "Выберите проверенное подключение"},
	"reconcile_connection_help":                                                       {"Only active verified Stripe Test Mode connections are eligible. Events Read is checked during execution; missing permission remains visible as a gap.", "Доступны активные проверенные подключения Stripe Test Mode. Право Events Read проверяется при выполнении; его отсутствие останется видимым пробелом."},
	"reconcile_stripe_connection":                                                     {"Stripe connection", "Подключение Stripe"},
	"reconcile_connection_required":                                                   {"Choose a verified connection.", "Выберите проверенное подключение."},
	"reconcile_choose_window":                                                         {"Set a bounded window", "Задайте ограниченный период"},
	"reconcile_window_help":                                                           {"The server accepts up to 29 days of recent history and stops at least two minutes before now.", "Сервер принимает недавнюю историю за последние 29 дней и период, заканчивающийся не позже чем за две минуты до текущего времени."},
	"reconcile_from_utc":                                                              {"From (UTC)", "Начало (UTC)"},
	"reconcile_to_utc":                                                                {"To (UTC)", "Конец (UTC)"},
	"reconcile_utc_help":                                                              {"Enter times in UTC; seconds are preserved during review.", "Указывайте время в UTC; секунды сохраняются при проверке."},
	"reconcile_date_utc":                                                              {"Date (UTC)", "Дата (UTC)"},
	"reconcile_time_utc":                                                              {"Time HH:MM:SS (UTC)", "Время ЧЧ:ММ:СС (UTC)"},
	"reconcile_calendar_hint":                                                         {"Choose a date or use the keyboard. Time is entered separately in UTC.", "Выберите дату или воспользуйтесь клавиатурой. Время вводится отдельно в UTC."},
	"reconcile_picker_invalid":                                                        {"Enter a valid UTC date and time within the permitted window.", "Укажите корректные дату и время UTC в допустимом периоде."},
	"reconcile_max_events":                                                            {"Maximum events", "Максимум событий"},
	"reconcile_max_help":                                                              {"1–500 events, fetched in bounded pages of 25.", "1–500 событий, ограниченные страницы по 25."},
	"reconcile_audit_reason":                                                          {"Audit reason", "Причина для аудита"},
	"reconcile_reason_help":                                                           {"Explain why this extra historical scan is needed (12–500 characters).", "Объясните причину дополнительной исторической проверки (12–500 символов)."},
	"reconcile_no_connection":                                                         {"No eligible verified Stripe Test Mode connection. Verify the provider and automation first.", "Нет подходящего проверенного подключения Stripe Test Mode. Сначала проверьте провайдера и сценарий."},
	"reconcile_viewer_note":                                                           {"Viewer access can inspect coverage; an owner or operator requests scans.", "Наблюдатель может просматривать покрытие; запросить проверку могут владелец и оператор."},
	"reconcile_requests_eyebrow":                                                      {"Durable requests", "Сохранённые запросы"},
	"reconcile_requests_title":                                                        {"Historical requests", "Исторические запросы"},
	"reconcile_requests_intro":                                                        {"Status and counts reflect persisted progress; no percentage is inferred.", "Статус и количество отражают сохранённый прогресс; процент не вычисляется предположительно."},
	"reconcile_scanned_limit":                                                         {"Scanned / limit", "Проверено / лимит"},
	"reconcile_discovered":                                                            {"Discovered", "Найдено"},
	"reconcile_finished":                                                              {"Finished", "Завершено"},
	"reconcile_request_details":                                                       {"Request details", "Детали запроса"},
	"reconcile_operator":                                                              {"Operator ID", "ID оператора"},
	"reconcile_request_id":                                                            {"Request ID", "ID запроса"},
	"reconcile_older_requests":                                                        {"Older requests", "Более ранние запросы"},
	"reconcile_no_requests":                                                           {"No historical scan has been requested.", "Исторические проверки ещё не запрашивались."},
	"reconcile_preview_eyebrow":                                                       {"Review before submission", "Проверка перед отправкой"},
	"reconcile_preview_title":                                                         {"Review historical scan", "Проверить историческую сверку"},
	"reconcile_preview_intro":                                                         {"Confirm the exact UTC bounds and notification policy. Submission creates a durable request; it does not prove coverage yet.", "Подтвердите точные границы UTC и политику уведомлений. Отправка создаёт сохранённый запрос, но ещё не подтверждает покрытие."},
	"reconcile_effective_bounds":                                                      {"Effective bounds", "Итоговые границы"},
	"reconcile_not_started":                                                           {"Not started", "Не начато"},
	"reconcile_up_to_pages":                                                           {"up to provider pages", "не более страниц провайдера"},
	"reconcile_preview_limit_note":                                                    {"Actual event count and provider permissions are known only during execution. Gaps remain visible.", "Фактическое число событий и права провайдера станут известны при выполнении. Пробелы останутся видимыми."},
	"reconcile_request_scan":                                                          {"Start historical check", "Начать проверку истории"},
	"reconcile_edit_bounds":                                                           {"Edit bounds", "Изменить границы"},
	"reconcile_state_healthy":                                                         {"Healthy checkpoint", "Контрольная точка без известных проблем"},
	"reconcile_state_body_healthy":                                                    {"A scan completed and no currently recorded gap or exception is present. This does not certify all historical Stripe activity.", "Проверка завершена без зарегистрированных пробелов и исключений. Это не гарантирует полноту всей истории Stripe."},
	"reconcile_state_unverified":                                                      {"Not yet verified", "Ещё не подтверждено"},
	"reconcile_state_body_unverified":                                                 {"There is no completed checkpoint. Coverage remains unknown.", "Нет завершённой контрольной точки. Покрытие неизвестно."},
	"reconcile_state_degraded":                                                        {"Needs attention", "Требует внимания"},
	"reconcile_state_body_degraded":                                                   {"A coverage gap, unresolved effect or delayed work needs review.", "Пробел покрытия, нерешённое действие или задержка требуют проверки."},
	"reconcile_state_blocked":                                                         {"Execution blocked", "Выполнение заблокировано"},
	"reconcile_state_body_blocked":                                                    {"Restore safety or provider access prevents a normal healthy state.", "Защита восстановления или доступ к провайдеру мешают нормальной обработке."},
	"checkpoint_event_discovery":                                                      {"Stripe event discovery", "Обнаружение событий Stripe"},
	"checkpoint_known_state":                                                          {"Known-customer state", "Состояние известных клиентов"},
	"checkpoint_historical":                                                           {"Historical scan", "Историческая проверка"},
	"request_pending":                                                                 {"Pending", "Ожидает"},
	"request_completed":                                                               {"Completed", "Завершён"},
	"request_blocked":                                                                 {"Blocked", "Заблокирован"},
	"request_limit_reached":                                                           {"Limit reached", "Достигнут лимит"},
	"stage_action_pending":                                                            {"Action planned.", "Действие запланировано."},
	"stage_action_queued":                                                             {"Queued for execution.", "Поставлено в очередь."},
	"stage_action_running":                                                            {"Provider action is running.", "Действие провайдера выполняется."},
	"stage_action_retry_wait":                                                         {"Automatic retry is scheduled.", "Автоматический повтор запланирован."},
	"stage_action_blocked_auth":                                                       {"Provider credentials need attention.", "Требуется проверить учётные данные провайдера."},
	"stage_action_blocked_config":                                                     {"Configuration blocks this action.", "Настройка блокирует действие."},
	"stage_action_needs_attention":                                                    {"Operator review is required.", "Нужна проверка оператора."},
	"stage_action_unknown":                                                            {"The external outcome is unknown.", "Внешний результат неизвестен."},
	"stage_action_dead":                                                               {"Automatic retries stopped.", "Автоматические повторы остановлены."},
	"stage_action_paused":                                                             {"Outbound action is paused.", "Исходящее действие приостановлено."},
	"stage_action_succeeded":                                                          {"Provider result is confirmed.", "Результат провайдера подтверждён."},
	"stage_action_skipped":                                                            {"Action was intentionally skipped.", "Действие намеренно пропущено."},
	"stage_action_superseded":                                                         {"Replaced by newer work.", "Заменено более новой задачей."},
	"reconcile_outbox_age":                                                            {"Outbox age", "Возраст очереди отправки"},
	"reconcile_review_bounds":                                                         {"Review scan bounds", "Проверить границы"},
	"reconcile_invalid_bounds":                                                        {"Check the UTC time window, event limit and audit reason, then try again.", "Проверьте период UTC, лимит событий и причину для аудита, затем повторите."},
	"reconcile_connection_unavailable":                                                {"This connection or automation is no longer eligible. Refresh and verify it before requesting a scan.", "Подключение или сценарий больше не подходит. Обновите страницу и проверьте их перед запросом."},
	"checkpoint_events":                                                               {"Stripe event discovery", "Обнаружение событий Stripe"},
	"checkpoint_known":                                                                {"Known-customer state", "Состояние известных клиентов"},
	"confirm_dialog_target":                                                           {"Target", "Объект"},
	"confirm_dialog_cancel":                                                           {"Cancel", "Отмена"},
	"confirm_pause_body":                                                              {"New events will stop creating outbound work until this automation is resumed.", "Новые события перестанут создавать исходящие задачи до возобновления сценария."},
	"confirm_activate_body":                                                           {"Activate this reviewed revision for new matching Stripe Test Mode events.", "Активировать проверенную версию для новых подходящих тестовых событий Stripe."},
	"confirm_mapping_body":                                                            {"Bind this verified Contact identity to the managed customer key. The audit reason is retained.", "Связать проверенный контакт с управляемым ключом клиента. Причина останется в аудите."},
	"confirm_unknown_body":                                                            {"Confirm delivery only after checking the exact Slack channel and message timestamp.", "Подтверждайте доставку только после проверки точного канала Slack и времени сообщения."},
	"confirm_reconcile_body":                                                          {"Queue the bounded historical scan shown above. CRM may be updated; Slack notifications will not be sent retroactively.", "Поставить показанную ограниченную проверку в очередь. CRM может обновиться; уведомления Slack задним числом не отправляются."},
	"runs_loading":                                                                    {"Loading matching runs…", "Загружаем подходящие запуски…"},
	"runs_request_error":                                                              {"The run list could not be refreshed. Retry the filter or open this page directly.", "Не удалось обновить список запусков. Повторите фильтр или откройте страницу напрямую."},
	"mapping_status_explain_matching":                                                 {"The Contact already has this managed customer key.", "У контакта уже записан этот управляемый ключ клиента."},
	"mapping_status_explain_unclaimed":                                                {"The Contact has no managed customer key; verify identity before binding.", "У контакта нет управляемого ключа клиента; проверьте личность перед связыванием."},
	"mapping_status_explain_conflict":                                                 {"The Contact belongs to another managed customer key. Binding is blocked.", "Контакт связан с другим управляемым ключом клиента. Связывание заблокировано."},
	"ui_error_session_title":                                                          {"Session expired", "Сеанс завершён"},
	"ui_error_session_body":                                                           {"Sign in again to continue. No action was submitted from this page.", "Войдите снова, чтобы продолжить. С этой страницы действие не отправлено."},
	"ui_error_signin_action":                                                          {"Sign in again", "Войти снова"},
	"ui_error_forbidden_title":                                                        {"Access denied", "Доступ запрещён"},
	"ui_error_forbidden_body":                                                         {"Your workspace role does not permit this action or page. Return to your workspaces to choose an allowed area.", "Ваша роль в рабочей области не позволяет открыть эту страницу или выполнить действие. Вернитесь к доступным областям."},
	"ui_error_csrf_title":                                                             {"Request not confirmed", "Запрос не подтверждён"},
	"ui_error_csrf_body":                                                              {"The page or session changed. Open the current page and submit the action again.", "Страница или сеанс изменились. Откройте актуальную страницу и отправьте действие снова."},
	"ui_error_not_found_title":                                                        {"Page not found", "Страница не найдена"},
	"ui_error_not_found_body":                                                         {"This item may have moved or is no longer available to your workspace.", "Объект мог измениться или больше недоступен вашей рабочей области."},
	"ui_error_unavailable_title":                                                      {"Service temporarily unavailable", "Сервис временно недоступен"},
	"ui_error_unavailable_body":                                                       {"The request could not be completed. Wait briefly, then reopen your workspace.", "Не удалось завершить запрос. Подождите немного и снова откройте рабочую область."},
	"ui_error_provider_title":                                                         {"Provider check unavailable", "Проверка сервиса недоступна"},
	"ui_error_provider_body":                                                          {"The provider or connection could not be checked now. Review its status in Connections before trying again.", "Сейчас не удалось проверить сервис или подключение. Перед повторной попыткой проверьте его состояние в Подключениях."},
	"ui_error_mapping_title":                                                          {"Contact mapping blocked", "Связывание контакта заблокировано"},
	"ui_error_mapping_body":                                                           {"The Contact identity or run state could not be confirmed. Reopen the run and inspect the Contact before trying again.", "Не удалось подтвердить идентичность контакта или состояние запуска. Откройте запуск и заново проверьте контакт перед повторной попыткой."},
	"ui_error_recovery_title":                                                         {"Recovery decision not applied", "Решение о восстановлении не применено"},
	"ui_error_recovery_body":                                                          {"The run changed while you were reviewing it. Reopen the run and check its current state before deciding again.", "Состояние запуска изменилось во время проверки. Откройте запуск и проверьте актуальное состояние перед новым решением."},
	"ui_error_input_title":                                                            {"Check the request", "Проверьте запрос"},
	"ui_error_input_body":                                                             {"The submitted values or page cursor are no longer valid. Reopen the page and review them.", "Отправленные значения или указатель страницы больше недействительны. Откройте страницу снова и проверьте их."},
	"ui_error_generic_title":                                                          {"Request could not be completed", "Не удалось завершить запрос"},
	"ui_error_generic_body":                                                           {"Reopen your workspace and try again. The action was not confirmed on this page.", "Откройте рабочую область снова и повторите попытку. На этой странице действие не подтверждено."},
	"ui_error_workspace_action":                                                       {"Open workspaces", "Открыть рабочие области"},
	"runs_auth_expired":                                                               {"Session expired. Sign in again to refresh runs.", "Сеанс завершён. Войдите снова, чтобы обновить запуски."},
}

func tr(lang, key string) string {
	copy, ok := interfaceCopy[key]
	if !ok {
		return key
	}
	if lang == "ru" && copy[1] != "" {
		return copy[1]
	}
	return copy[0]
}

// Operational instants stay in UTC; only their presentation changes.
func displayTime(lang string, instant time.Time, seconds bool) string {
	if seconds {
		if lang == "ru" {
			return instant.UTC().Format("02.01.2006 15:04:05 UTC")
		}
		return instant.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	if lang == "ru" {
		return instant.UTC().Format("02.01.2006 15:04 UTC")
	}
	return instant.UTC().Format("2006-01-02 15:04 UTC")
}

func displayDay(lang, utcDay string) string {
	if lang != "ru" {
		return utcDay
	}
	day, err := time.Parse("2006-01-02", utcDay)
	if err != nil {
		return utcDay
	}
	return day.Format("02.01.2006")
}

func displayCount(lang string, count int64, form string) string {
	digits := strconv.FormatInt(count, 10)
	separator := ","
	if lang == "ru" {
		separator = "\u00a0"
	}
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + separator + digits[i:]
	}
	if form != "run" {
		return digits
	}
	if lang != "ru" {
		if count == 1 {
			return digits + " run"
		}
		return digits + " runs"
	}
	word := "запусков"
	if count%100 < 11 || count%100 > 14 {
		switch count % 10 {
		case 1:
			word = "запуск"
		case 2, 3, 4:
			word = "запуска"
		}
	}
	return digits + " " + word
}

func workspacePageCount(lang string, count int) string {
	if lang != "ru" {
		return strconv.Itoa(count) + " workspaces on this page"
	}
	word := "областей"
	if count%100 < 11 || count%100 > 14 {
		switch count % 10 {
		case 1:
			word = "область"
		case 2, 3, 4:
			word = "области"
		}
	}
	return strconv.Itoa(count) + " " + word + " на этой странице"
}

func displayDuration(lang string, seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	parts := []struct {
		value int64
		en    [2]string
		ru    [3]string
	}{
		{seconds / 86400, [2]string{"day", "days"}, [3]string{"день", "дня", "дней"}},
		{seconds / 3600 % 24, [2]string{"hour", "hours"}, [3]string{"час", "часа", "часов"}},
		{seconds / 60 % 60, [2]string{"minute", "minutes"}, [3]string{"минута", "минуты", "минут"}},
		{seconds % 60, [2]string{"second", "seconds"}, [3]string{"секунда", "секунды", "секунд"}},
	}
	var out []string
	for i, part := range parts {
		if part.value == 0 && (seconds != 0 || i != len(parts)-1) {
			continue
		}
		word := part.en[1]
		if lang == "ru" {
			word = part.ru[2]
			if part.value%100 < 11 || part.value%100 > 14 {
				switch part.value % 10 {
				case 1:
					word = part.ru[0]
				case 2, 3, 4:
					word = part.ru[1]
				}
			}
		} else if part.value == 1 {
			word = part.en[0]
		}
		out = append(out, strconv.FormatInt(part.value, 10)+" "+word)
		if len(out) == 2 {
			break
		}
	}
	return strings.Join(out, " ")
}
