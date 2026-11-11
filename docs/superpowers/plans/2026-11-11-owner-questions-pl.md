# Pytania do Ownera do planu rozwoju z 11 listopada 2026

Dotyczy: [plan rozwoju](2026-11-11-development-plan.md).

Każde pytanie ma **domyślną odpowiedź**, którą plan przyjął. Jeśli domyślna
pasuje, nie musisz nic robić. Odpowiedz tylko tam, gdzie jest inaczej: wpisz
odpowiedź pod pytaniem i zrób commit, albo podaj numery w rozmowie.

Pytania oznaczone **(blokuje)** wstrzymują wskazane karty planu, dopóki nie
odpowiesz. Reszta planu (M0 do M6) idzie bez nich.

## A. Produkt i firma

**Q1 (blokuje M7).** Co dokładnie znaczy DaaS w Twoim produkcie: Data as a
Service, Desktop as a Service czy Device as a Service?
Domyślnie: Data as a Service. Od tego zależy cała funkcja „operate”.

**Q2 (blokuje M7-T4).** W jakiej formie klient dostaje produkt: API, pliki,
feed, dashboard, dostęp do bazy?
Domyślnie: API i pliki.

**Q3 (blokuje M7-T4).** Skąd pochodzą dane, jak często się odświeżają i co
obiecujesz klientom (świeżość, kompletność, dostępność)?
Domyślnie: brak formalnego SLA; system mierzy świeżość i kompletność, a progi
ustalasz Ty.

**Q4 (blokuje M7-T4).** Gdzie produkt działa (chmura, dostawca, własny serwer)
i czym jest dziś monitorowany?
Domyślnie: nieznane; karta czeka.

**Q5 (blokuje M8 dla produktu).** Jak dziś wygląda wdrożenie produktu: ręcznie,
CI/CD, jakie środowiska?
Domyślnie: nieznane; karta czeka.

**Q6.** Gdzie jest kod produktu: GitHub czy Azure DevOps, ile repozytoriów,
jakie języki i jak się uruchamia testy?
Domyślnie: jedno prywatne repozytorium na GitHubie, osobny Projekt w Summa42.

**Q7.** Czy poza Tobą ktoś pracuje w firmie lub przy produkcie (ludzie,
podwykonawcy)? Czy mają dostawać zadania lub decyzje od systemu?
Domyślnie: nie; jedynym człowiekiem jest Owner.

**Q8 (blokuje M7-T3).** Jakie masz zobowiązania cykliczne z terminem (odnowienia
domen i certyfikatów, raporty, podatki, faktury, aktualizacje zależności)?
Które jest najlepszym pierwszym kandydatem?
Domyślnie: comiesięczna aktualizacja zależności w repozytorium Summa42.

**Q9 (blokuje M7-T5).** Którędy przychodzą zgłoszenia klientów (e-mail,
helpdesk, formularz, Slack, Discord)?
Domyślnie: e-mail.

**Q10 (blokuje M7-T6).** Czym fakturujesz i gdzie trzymasz klientów (Stripe,
Fakturownia, inny system, arkusz)?
Domyślnie: nieznane; karta czeka.

**Q11.** Czy system może kiedykolwiek sam wysłać coś do klienta albo wystawić
dokument finansowy bez Twojego zatwierdzenia?
Domyślnie: nie. Takie operacje mają co najmniej ryzyko MEDIUM i są poza
każdym mandatem automatyzacji, dopóki wyraźnie tego nie zmienisz.

**Q12.** Czy w danych produktu lub zgłoszeniach są dane osobowe albo dane
klientów objęte umową o poufności? Czy mogą trafiać do modeli w chmurze?
Domyślnie: prywatne źródła tylko przez modele z zerową retencją (ZDR) albo
lokalne.

## B. Projekty

**Q13.** Jakie Projekty mają powstać na początku?
Domyślnie: dwa: `summa42` (to repozytorium, poligon) i `daas` (produkt).

**Q14 (blokuje M2-T7).** Jakie ma być drugie, niegithubowe źródło pracy?
Domyślnie: lokalna skrzynka plików (każdy plik Markdown to jedno zadanie).
Alternatywy: work itemy Azure DevOps, Jira, Linear.

**Q15.** Czy Projekty mogą dzielić budżet?
Domyślnie: nie; każdy ma własną kopertę pod kopertą Kolektywu.

**Q16.** Czy Projekt ma mieć własnych maintainerów innych niż Owner?
Domyślnie: nie.

**Q17.** Kiedy praca usługowa dla klientów (osobne umowy, terminy, faktury per
klient)?
Domyślnie: poza tym planem.

**Q18.** Czy wszystko działa na jednym hoście?
Domyślnie: tak, jeden host, jedna baza SQLite.

**Q19.** Czy nazwa „Project” jest w porządku dla zakresu poniżej Kolektywu?
Domyślnie: tak.

## C. Budżet i modele

**Q20 (blokuje M3-T1).** Jakie limity pieniężne na start?
Domyślnie: 5 USD na case i 20 USD dziennie na Kolektyw (z roadmapy), do
kalibracji po pilotażu.

**Q21 (blokuje M3-T1).** Jakie subskrypcje i dostępy do modeli masz (Gemini,
Claude, Codex, Copilot, Command Code, Ollama lokalnie)?
Domyślnie: te, które README wymienia jako potwierdzone.

**Q22.** Czy wolno używać płatnych modeli przez API poza subskrypcjami?
Domyślnie: tylko w ramach jawnego limitu z Q20.

**Q23.** Jak będzie uruchamiany Gemini Flash: przez Antigravity (`agy`),
Gemini CLI czy API? Który model ma recenzować jego pracę?
Domyślnie: producent przez profil `agy`; recenzent to inny model (Claude albo
Codex). Recenzent nie może być tym samym modelem co producent.

## D. Ciągłość

**Q24 (blokuje M3-T3).** Którym kanałem system ma Cię powiadamiać?
Domyślnie: webhook Discorda (most Discord już istnieje). Alternatywy: e-mail,
ntfy, Telegram.

**Q25.** Jak długo system może pracować bez Twojego znaku życia?
Domyślnie: 7 dni (obecne ustawienie).

**Q26 (blokuje M4-T4, a przez to M5).** Jak chronić klucz Ownera?
Domyślnie: klucz zaszyfrowany hasłem, odszyfrowywany tylko w CLI na czas
jednej podpisanej akcji. Mocniejsza opcja: klucz sprzętowy (YubiKey) albo
`ssh-agent`.

**Q27.** Gdzie i jak często robić kopie zapasowe?
Domyślnie: codziennie do lokalnego katalogu; kopię poza host przenosisz sama,
dopóki nie wskażesz miejsca.

**Q28.** Czy docelowy host to zwykły Linux z systemd, czy WSL?
Domyślnie: Linux z systemd. Na WSL izolacja użytkownika modelu jest niepełna
(README).

**Q29.** Ile Twojej uwagi miesięcznie jest celem i ile zajmuje to dziś?
Domyślnie: cel około godziny miesięcznie; stan obecny zmierzy pilotaż (M1-T5).

## E. Automatyzacja

**Q30 (blokuje M6-T1).** Po ilu identycznych zatwierdzeniach system ma
zaproponować stałą delegację?
Domyślnie: 5 zatwierdzeń tej samej klasy w 30 dni, zero odrzuceń.

**Q31.** Ile zgodnych przypadków w trybie cienia, zanim propozycja trafi do
Ciebie albo zostanie przyjęta?
Domyślnie: 5 kolejnych zgodnych; jedna niezgodność zeruje licznik.

**Q32 (blokuje M6-T4).** Co system może na start przejmować sam, bez pytania?
Domyślnie: tylko rzeczy o ryzyku LOW, odwracalne i bez skutku na zewnątrz
(harmonogramy zadań tylko do odczytu, raporty, porządki wewnętrzne). Wszystko
inne jest propozycją do zatwierdzenia.

**Q33.** Czego nie wolno delegować nigdy?
Domyślnie: polityki, kluczy, listy zatwierdzających, budżetów, plików
workflow CI, samego mechanizmu delegacji i mandatu, komunikacji z klientami i
dokumentów finansowych (Q11).

**Q34.** Czy model językowy może podpowiadać kandydatów do automatyzacji?
Domyślnie: dopiero po działającej wersji deterministycznej (M6-T6), i tylko
jako kandydat przechodzący tę samą ścieżkę.

**Q35.** Jak często chcesz dostawać podsumowanie (co przejęto, co
zaproponowano, co zawieszono, ile wydano)?
Domyślnie: raz w tygodniu.

## F. Repozytorium i proces

**Q36.** Kto pisze specyfikacje kart `DESIGN`?
Domyślnie: mocniejszy model niż wykonawca, a Ty je zatwierdzasz. Gemini Flash
wykonuje karty `BUILD`.

**Q37.** Czy do osiągnięcia A2 każdy merge robisz Ty?
Domyślnie: tak.

**Q38.** Co z otwartym PR #176 (Dapr i AgentLedger)?
Domyślnie: zamknąć bez merge i zostawić jako notatkę badawczą, bo żadna
działająca ścieżka go nie wywołuje (ta sama reguła co przy zaparkowanych
spike'ach).

**Q39.** Co z epikami „Post-MVC” (#3, #5, #11, #12, #15, #16, #17, #60)?
Domyślnie: zostają otwarte i poza tym planem; wracają, gdy karta planu ich
potrzebuje.

**Q40.** Czy kolejność „Projekty przed ciągłością” jest w porządku?
Domyślnie: tak, bo budżety, powiadomienia i pauzy są per Projekt. Jeśli wolisz
najpierw powiadomienia i kopie zapasowe, M3-T3 do M3-T5 można zrobić przed M2.

**Q41.** Gdzie ma żyć konfiguracja Twojej firmy (manifesty Projektów,
zobowiązania, mandaty)?
Domyślnie: poza publicznym repozytorium, w `SUMMA42_HOME`; w repozytorium są
tylko przykłady.

**Q42.** W jakim języku dokumenty w repozytorium?
Domyślnie: angielski; ten plik jest po polsku, bo jest dla Ciebie.
