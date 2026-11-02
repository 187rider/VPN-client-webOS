(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  // QR pairing stores the PIN only in URL fragment, never in the server request.
  // Remove it from the address bar after extracting it.
  var qrPin = '';
  try {
    var match = location.hash.match(/^#pair=(\d{6})$/);
    if (match) { qrPin = match[1]; history.replaceState(null, '', location.pathname + location.search); }
  } catch (ignored) {}
  var accessKey = sessionStorage.getItem('cvpn_webui_key') || '';
  var locale=window.CVPNLocale;
  function syncLanguagePreference(){
    if(!locale)return;
    api('language').then(function(result){
      try { if(!localStorage.getItem('cvpn_webos_language'))locale.set(result.language||'auto'); }catch(e){}
    }).catch(function(){});
  }

  var csrf = ''; var pairPending = null; var pairingTimer = null;
  var allProfiles = {items:[],groups:[],active:''};
  var currentTab = 'home';
  var currentStatus = null, busy = false, polling = false, modalGroup = null, modalServer = null;
  var pendingAction = '', pendingTarget = '';
  var errorMessage = '', errorUntil = 0, lastKnownOnline = true, modalReturnFocus = null;
  var collapsed = {};
  var subscriptionProbeResults={};
  try { collapsed = JSON.parse(localStorage.getItem('cvpn_connect_collapsed') || '{}') || {}; } catch (e) {collapsed={};}
  var localeNumber = function(n){if(locale&&locale.language()==='en')return n+(n===1?' server':' servers');return n+' '+((n%10===1 && n%100!==11)?'сервер':((n%10>=2&&n%10<=4&&(n%100<12||n%100>14))?'сервера':'серверов'));};
  function visible(id, yes){ var el=$(id); if(el) el.hidden=!yes; }
  function alertUser(text, bad){var t=$('notice');t.textContent=text;t.className='toast'+(bad?' error':'');t.hidden=false;clearTimeout(alertUser.timer);alertUser.timer=setTimeout(function(){t.hidden=true;},7000);}
  function showLogin(){switchTab('home');visible('settings-trigger',false);accessKey='';csrf='';sessionStorage.removeItem('cvpn_webui_key');visible('login',true);visible('dashboard',false);$('settings-trigger').disabled=true;closeModal();}
  function api(endpoint,method,body){
    var headers={'Content-Type':'application/json'};
    if(accessKey){headers['X-CVPN-Key']=accessKey;}
    else if((method||'GET')!=='GET'&&csrf){headers['X-CVPN-CSRF']=csrf;}
    return fetch('/api/'+endpoint,{method:method||'GET',headers:headers,credentials:'same-origin',cache:'no-store',body:body===undefined?undefined:JSON.stringify(body)}).then(function(res){return res.text().then(function(txt){var json;try{json=JSON.parse(txt);}catch(e){throw Error('Сервер вернул некорректный ответ');}if(res.status===401&&endpoint!=='auth'){showLogin();throw Error('Устройство не авторизовано. Откройте CVPN на LG и введите код.');}if(!res.ok){var err=new Error(json.error||'Ошибка сервера');err.issues=json.issues||[];throw err;}return json;});});
  }
  function requestPair(path,method,data){return fetch('/api/'+path,{method:method||'GET',credentials:'same-origin',headers:{'Content-Type':'application/json'},body:data===undefined?undefined:JSON.stringify(data),cache:'no-store'}).then(function(r){return r.json().then(function(v){if(!r.ok)throw Error(v.error||'Ошибка сопряжения');return v;});});}
  function authenticate(){
    return api('auth').then(function(a){csrf=a.csrf||'';accessKey=a.legacy?accessKey:'';return refresh().then(function(r){syncLanguagePreference();return r;});}).catch(function(){showLogin();});
  }
  function syncPIN(){
    var input=$('pairing-code');if(!input)return;
    var code=String(input.value||'').replace(/\D/g,'').slice(0,6);
    if(input.value!==code)input.value=code;
    var cells=document.querySelectorAll('.otp-cells span');
    Array.prototype.forEach.call(cells,function(cell,i){cell.textContent=code.charAt(i);cell.classList.toggle('filled',i<code.length);});
  }
  $('pairing-code').addEventListener('input',syncPIN);
  function stopPairPoll(){if(pairingTimer){clearInterval(pairingTimer);pairingTimer=null;}pairPending=null;$('pair-button').disabled=false;}
  function pollPair(){if(!pairPending)return;
    requestPair('pairing/status?id='+encodeURIComponent(pairPending.id)+'&token='+encodeURIComponent(pairPending.token)).then(function(r){
      if(r.state==='approved'){stopPairPoll();$('pairing-progress').textContent='Телефон подключён!';authenticate();}
    }).catch(function(e){stopPairPoll();$('login-error').textContent=e.message;$('pairing-progress').textContent='Откройте CVPN на телевизоре и попробуйте ещё раз.';});
  }
  // Synchronize action buttons after every status change and every operation.
  // A previously selected server must remain clickable if the VPN is OFF.
  // During an in-flight operation all connection buttons are temporarily locked
  // to avoid overlapping network changes. Once it finishes, only the active
  // server remains unclickable while the VPN is connected.
  function syncServerButtons(){
    var connected=!!(currentStatus&&currentStatus.connected);
    var rows=document.querySelectorAll('.server-row');
    Array.prototype.forEach.call(rows,function(row){
      var main=row.querySelector('.server-main');
      if(!main)return;
      var selected=row.getAttribute('data-id')===allProfiles.active;
      var alreadyActive=connected&&selected;
      main.disabled=busy||alreadyActive;
      var name=main.getAttribute('data-server-name')||'сервер';
      var label=alreadyActive?('Сервер '+name+' уже подключён'):
        (connected?('Переключиться на '+name):('Подключить '+name));
      main.setAttribute('aria-label',label);
      main.title=alreadyActive?'Уже подключён':(busy?'Дождитесь завершения операции':label);
    });
  }
  function setBusy(yes){busy=yes;$('subs-add').disabled=yes;$('add-toggle').disabled=yes;$('refresh').disabled=yes;$('settings-trigger').disabled=yes;$('disconnect').disabled=yes;$('confirm').disabled=yes;$('import-submit').disabled=yes;$('retry').disabled=yes;syncServerButtons();}
  function updatePending(action,target){pendingAction=action||'';pendingTarget=target||'';if(currentStatus)renderStatus(currentStatus);}
  function displayError(text){errorMessage=text;errorUntil=Date.now()+14000;alertUser(text,true);renderStatus(currentStatus);}
  function currentProfile(){return (allProfiles.items||[]).filter(function(p){return p.id===allProfiles.active;})[0]||null;}
  function isOnline(status){return !!(status&&status.connected);}
  function renderStatus(s){
    if(!s){return;}currentStatus=s;
    var connected=isOnline(s),needsRecovery=!!s.needsRecovery,ending=connected&&s.mode==='trial'&&s.trialRemainingSec===0;
    var ready=s.phase==='ready',errorActive=Date.now()<errorUntil;
    var name=currentProfile();
    var statusCard=$('status-card');statusCard.className='status-card'+(needsRecovery?' error':connected?(ready?' connected':' loading'):(errorActive?' error':'')+(pendingAction?' is-pending':''));
    visible('connection-step',!!pendingAction);
    if(pendingAction){$('connection-step-text').textContent=pendingAction==='connecting'?'Устанавливаем безопасное соединение':pendingAction==='checking'?'Проверяем состояние VPN':pendingAction==='disconnecting'?'Возвращаем обычную сеть':'Подтверждаем подключение';}
    if(connected){
      $('connection-state').textContent=ending?'Завершение подключения…':(ready?'Вы защищены':'Подключение…');
      $('connection-detail').textContent=ending?'Восстанавливаем обычную сеть':(ready?'VPN работает на телевизоре':'Проверяем соединение и DNS…');
    }else if(needsRecovery){$('connection-state').textContent='Требуется восстановление сети';$('connection-detail').textContent=s.reason||'Состояние VPN требует проверки. Отключите соединение для безопасного восстановления.';}else if(errorActive){$('connection-state').textContent='Не удалось подключиться';$('connection-detail').textContent=errorMessage;}
    else{$('connection-state').textContent='VPN отключён';$('connection-detail').textContent=name?'Нажмите на сервер для подключения':'Добавьте свою первую конфигурацию';}
    if(pendingAction){$('connection-state').textContent=pendingAction==='disconnecting'?'Отключаем VPN…':pendingAction==='confirming'?'Подтверждаем…':pendingAction==='checking'?'Проверяем подключение…':'Подключаем VPN…';$('connection-detail').textContent=pendingAction==='disconnecting'?'Телевизор вернётся к обычной сети':pendingTarget||'Подготавливаем соединение';}
    visible('active-server',connected&&!!name);if(name){$('active-server-name').textContent=name.name;}
    visible('disconnect',connected||needsRecovery);$('disconnect').textContent=needsRecovery?'Восстановить сеть':'Отключить';$('disconnect').disabled=busy||ending;
    visible('retry',!connected&&errorActive&&!!name);$('retry').disabled=busy;
    visible('trial-hint',connected&&s.mode==='trial'&&ready&&!ending);
    if(connected&&s.mode==='trial'){
      var rem=typeof s.trialRemainingSec==='number'?(' · '+s.trialRemainingSec+' сек.'):' · до автоотключения';
      $('trial-label').textContent='Пробное подключение'+rem;
    }
    $('panel-health').className='online-mini'+(connected?' ready':'');
    $('panel-health').textContent=needsRecovery?'Проверка сети':connected?'VPN активен':'Локальная панель';
    $('engine-version').textContent=s.engine||'CVPN Engine';
    $('dns-status').textContent=connected?'Системный DNS через TUN (DoH не подтверждён)':'Системный DNS';
    $('session-mode').textContent=connected?(s.mode==='trial'?'Пробный режим':'Подключено'):'Отключено';
    var cards=document.querySelectorAll('.server-row');
    Array.prototype.forEach.call(cards,function(row){
      var id=row.getAttribute('data-id');row.classList.toggle('selected',id===allProfiles.active);
      row.classList.toggle('live',connected&&ready&&id===allProfiles.active);
      var tag=row.querySelector('.server-selection');if(tag)tag.textContent=(connected&&ready&&id===allProfiles.active)?'Активен':(id===allProfiles.active?'Выбран':'');
    });
    syncServerButtons();
  }
  // Local browser metadata is only a friendly display label. It grants no privileges.
  // The server sanitizes the name and binds the session to a valid TV PIN.
  function detectedDeviceName(){
    var ua=navigator.userAgent||'', platform=navigator.userAgentData&&navigator.userAgentData.platform||'';
    var os=/Android/i.test(ua+platform)?'Android':/iPhone|iPad|iPod/i.test(ua+platform)?'iPhone / iPad':/Windows/i.test(ua+platform)?'Windows':/Mac/i.test(ua+platform)?'macOS':/Linux/i.test(ua+platform)?'Linux':'Устройство';
    var browser=/Edg\//.test(ua)?'Edge':/SamsungBrowser\//.test(ua)?'Samsung Internet':/Firefox\//.test(ua)?'Firefox':/CriOS|Chrome\//.test(ua)?'Chrome':/Safari\//.test(ua)?'Safari':'браузер';
    return (os+' · '+browser).slice(0,50);
  }
  function isOfficialCVPN(g){
    return !!(g && g.kind==='subscription' && g.officialCVPN===true);
  }
  function providerDate(value){
    var dt=new Date(value);if(isNaN(dt.getTime()))return '';
    return dt.toLocaleDateString(locale&&locale.language()==='en'?'en-GB':'ru-RU',{day:'2-digit',month:'short',year:'numeric'});
  }
  function isEN(){return !!(locale&&locale.language()==='en');}
  // Provider-supplied metadata is text only; URLs are already validated on the backend.
  function renderCVPNMeta(g){
    if(!isOfficialCVPN(g))return null;
    var meta=g.meta||{}, en=isEN();
    var block=document.createElement('div');block.className='cvpn-provider-meta';
    var fields=document.createElement('div');fields.className='cvpn-provider-fields';
    function field(symbol,label,value,href){
      var item=document.createElement(href?'a':'span');item.className='cvpn-provider-item';
      if(href){item.href=href;item.target='_blank';item.rel='noopener noreferrer';}
      var icon=document.createElement('span');icon.className='cvpn-provider-icon';icon.setAttribute('aria-hidden','true');icon.textContent=symbol;
      var cap=document.createElement('span');cap.className='cvpn-provider-label';cap.textContent=label;
      var txt=document.createElement('strong');txt.textContent=value;
      item.appendChild(icon);item.appendChild(cap);item.appendChild(txt);fields.appendChild(item);
    }
    if(meta.supportUrl){var tg=/^https:\/\/(?:t\.me|telegram\.me)\//i.test(meta.supportUrl);field('↗',tg?'Telegram':(en?'Support':'Поддержка'),tg?'Telegram':(en?'Open':'Открыть'),meta.supportUrl);}
    if(meta.updateHours){field('↻',en?'Suggested refresh':'Интервал сервиса',String(meta.updateHours)+(en?' h':' ч'));}
    if(meta.expiresAt){var date=providerDate(meta.expiresAt),expired=new Date(meta.expiresAt).getTime()<Date.now();if(date)field('◷',en?'Valid until':'Действует до',date+(expired?(en?' · expired':' · истекла'):''));}
    else if(meta.neverExpires){field('◷',en?'Valid until':'Действует до',en?'No expiry':'Без срока');}
    if(meta.totalBytes>0){var gb=function(v){return (v/1073741824).toFixed(1).replace(/\.0$/,'')};field('◉',en?'Traffic used':'Использовано',gb(meta.usedBytes)+' / '+gb(meta.totalBytes)+' GB');}
    else if(meta.unlimitedTraffic){field('◉',en?'Traffic':'Трафик',en?'Unlimited':'Без лимита');}
    if(meta.portalUrl){field('↗',en?'Website':'Сайт',en?'Open':'Открыть',meta.portalUrl);}
    if(fields.children.length){block.appendChild(fields);}
    else {var note=document.createElement('p');note.className='cvpn-provider-note';note.textContent=en?'Provider details appear here if published. Tap refresh ↻ to check.':'Данные сервиса появятся здесь, если он их передаёт. Нажмите ↻ для проверки.';block.appendChild(note);}
    if(meta.announcement){var announcement=document.createElement('p');announcement.className='cvpn-provider-announcement';announcement.textContent=meta.announcement;block.appendChild(announcement);}
    // An advertised refresh interval is not a running auto-update scheduler.
    if(meta.updateHours){var caption=document.createElement('small');caption.className='cvpn-provider-footnote';caption.textContent=en?'Interval recommended by the provider; update manually using ↻.':'Интервал рекомендован сервисом; обновление выполняется кнопкой ↻.';block.appendChild(caption);}
    return block;
  }
  function probeSubscription(g,button){
    if(busy)return;
    if(currentStatus&&currentStatus.connected){alertUser(isEN()?'Disconnect VPN before checking latency.':'Отключите VPN перед проверкой задержки.',true);return;}
    button.disabled=true;button.classList.add('is-pinging');
    api('groups/ping','POST',{id:g.id}).then(function(result){
      var groupResults={};(result.results||[]).forEach(function(item){groupResults[item.id]=item;});
      subscriptionProbeResults[g.id]=groupResults;
      renderProfiles(allProfiles);
      alertUser(isEN()?'TCP latency measured from your TV.':'TCP-задержка измерена с телевизора.');
    }).catch(function(err){alertUser(err.message,true);}).then(function(){button.disabled=false;button.classList.remove('is-pinging');});
  }
  function makeGroupAvatar(g,cssClass){
    var avatar=document.createElement('span');avatar.className=cssClass;
    if(isOfficialCVPN(g)){
      avatar.classList.add('cvpn-official');
      var image=document.createElement('img');image.src='/cvpn-brand.png?v=rc41';image.alt='';image.loading='lazy';avatar.appendChild(image);
    }else{avatar.textContent=(g.name||'•').charAt(0).toUpperCase();}
    return avatar;
  }
  function humanUpdated(g){
    if(!g.updatedAt)return '';
    var dt=new Date(g.updatedAt);
    return isNaN(dt.getTime())?'':(locale&&locale.language()==='en'?'Updated '+dt.toLocaleDateString('en-US',{year:'numeric',month:'short',day:'numeric'}):'Обновлено '+dt.toLocaleDateString('ru-RU'));
  }
  // Prefer a provider-provided country flag. A generated flag is used only
  // when the name supplies a recognizable country or leading ISO code.
  // Never infer the exit location from the host IP or VPN transport.
  var flagPairRE=/[\u{1F1E6}-\u{1F1FF}]{2}/u;
  var countryAliases=[
    ['NL',/(?:нидерланд|голланди|netherlands|holland|амстердам|amsterdam|роттердам|rotterdam)/i],
    ['DE',/(?:германи|germany|deutschland|франкфурт|frankfurt|берлин|berlin|мюнхен|munich)/i],
    ['US',/(?:сша|соедин[её]нн.{0,5}штат|united states|america|америк|санта[ -]клара|santa[ -]clara|нью[ -]йорк|new[ -]york|лос[ -]анджелес|los[ -]angeles|чикаго|chicago)/i],
    ['RU',/(?:росси|russia|москва|moscow|петербург|petersburg)/i],
    ['FR',/(?:франци|france|париж|paris|марсел|marseille)/i],
    ['TR',/(?:турци|turkey|türkiye|turkiye|стамбул|istanbul|анкара|ankara)/i],
    ['GB',/(?:британи|англи|united kingdom|great britain|london|лондон)/i],
    ['SE',/(?:швец|sweden|стокгольм|stockholm)/i],
    ['FI',/(?:финлянди|finland|хельсинк|helsinki)/i],
    ['NO',/(?:норвеги|norway|осло|oslo)/i],
    ['DK',/(?:дани[яию]|denmark|копенгаген|copenhagen)/i],
    ['PL',/(?:польш|poland|варшав|warsaw)/i],
    ['CZ',/(?:чехи|czech|праг|prague)/i],
    ['AT',/(?:австри|austria|вен[аеыу]|vienna)/i],
    ['CH',/(?:швейцари|switzerland|zurich|цюрих)/i],
    ['IT',/(?:итали|italy|рим[ае]?\b|rome|милан|milan)/i],
    ['ES',/(?:испани|spain|мадрид|madrid|барселон|barcelona)/i],
    ['PT',/(?:португали|portugal|лиссабон|lisbon)/i],
    ['BE',/(?:бельги|belgium|брюссел|brussels)/i],
    ['IE',/(?:ирланд|ireland|дублин|dublin)/i],
    ['UA',/(?:украин|ukraine|киев|kyiv)/i],
    ['KZ',/(?:казахстан|kazakhstan|алмат|almaty)/i],
    ['BY',/(?:беларус|belarus|минск|minsk)/i],
    ['JP',/(?:япони|japan|токио|tokyo|осак|osaka)/i],
    ['KR',/(?:коре[яию]|south korea|сеул|seoul)/i],
    ['CN',/(?:кита[йяю]|china|пекин|beijing|шанхай|shanghai)/i],
    ['SG',/(?:сингапур|singapore)/i],
    ['HK',/(?:гонконг|hong kong)/i],
    ['TW',/(?:тайван|taiwan|тайбэй|taipei)/i],
    ['IN',/(?:инди[яию]|india|мумбаи|mumbai|дели|delhi)/i],
    ['AE',/(?:оаэ|emirates|дуба[йяе]|dubai|абу[ -]даби|abu dhabi)/i],
    ['IL',/(?:израил|israel|тель[ -]авив|tel aviv)/i],
    ['CA',/(?:канад|canada|торонто|toronto|ванкувер|vancouver)/i],
    ['BR',/(?:бразили|brazil|сан[ -]паулу|sao paulo)/i],
    ['AU',/(?:австрали|australia|сидне[йяе]|sydney)/i],
    ['NZ',/(?:нов[а-я]+[ -]зеланд|new zealand|окленд|auckland)/i],
    ['ID',/(?:индонези|indonesia|джакарт|jakarta)/i],
    ['TH',/(?:таиланд|тайланд|thailand|бангкок|bangkok)/i],
    ['VN',/(?:вьетнам|vietnam|ханой|hanoi|хошимин|ho chi minh)/i],
    ['RO',/(?:румыни|romania|бухарест|bucharest)/i],
    ['HU',/(?:венгри|hungary|будапешт|budapest)/i],
    ['GR',/(?:греци|greece|афин|athens)/i],
    ['EU',/(?:автовыбор|автоматическ.{0,12}выбор|automatic selection|auto select|европейск.{0,8}союз|european union)/i]
  ];
  // Explicit leading ISO-3166 codes are trusted only in a dedicated tag, not
  // in a random word (e.g. 'US' inside 'STATUS').
  var isoCodes='AD AE AF AG AI AL AM AO AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET EU FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW'.split(' ');
  function isoFlag(code){code=String(code||'').toUpperCase();if(isoCodes.indexOf(code)<0)return '';return String.fromCodePoint(127397+code.charCodeAt(0),127397+code.charCodeAt(1));}
  function countryFor(name){
    var n=String(name||''),explicit=n.match(flagPairRE);if(explicit)return explicit[0];
    var code=n.match(/^\s*(?:\[([A-Z]{2})\]|\(([A-Z]{2})\)|([A-Z]{2})(?=[ -·|:]))/);
    if(code){var flag=isoFlag(code[1]||code[2]||code[3]);if(flag)return flag;}
    for(var i=0;i<countryAliases.length;i++){if(countryAliases[i][1].test(n))return isoFlag(countryAliases[i][0]);}
    return '◉';
  }
  function shortServerName(p,group){
    var n=p.name||'Без названия',prefix=(group.name||'')+' · ';
    if(n.indexOf(prefix)===0)n=n.slice(prefix.length);
    // Only strip an actual flag that we now show as the leading icon. Keep
    // different flags, e.g. a transit country mentioned in the profile name.
    var flag=countryFor(n);
    if(flag!=='◉')n=n.split(flag).join('');
    return n.replace(/^\s*[·|—:-]\s*/,'').replace(/\s{2,}/g,' ').trim()||(p.name||'Сервер');
  }
  function renderProfiles(result){
    allProfiles=result||{items:[],groups:[],active:''};var target=$('profiles');target.textContent='';
    var items=(allProfiles.items||[]).filter(function(x){return x.groupId!=='installed'&&x.id!=='installed';}),groups=(allProfiles.groups||[]).filter(function(g){return g.kind!=='installed'&&g.id!=='installed';});
    renderSubscriptions();
    if(!items.length){var empty=document.createElement('div');empty.className='empty-state';empty.innerHTML='<strong>Нет конфигураций</strong><p>Добавьте подписку или VLESS-ссылку с телефона, чтобы начать.</p>';target.appendChild(empty);return;}
    if(!groups.length){groups=[{id:'manual',name:'Отдельные подключения',kind:'manual',count:items.length}];}
    groups.forEach(function(g){
      var list=items.filter(function(item){return item.groupId===g.id;});if(!list.length)return;
      var wrapper=document.createElement('article');wrapper.className='service-group'+(collapsed[g.id]?' is-collapsed':'');
      var head=document.createElement('div');head.className='group-head';
      var toggle=document.createElement('button');toggle.type='button';toggle.className='group-toggle';toggle.setAttribute('aria-expanded',collapsed[g.id]?'false':'true');
      var avatar=makeGroupAvatar(g,'group-avatar');
      var copy=document.createElement('span');copy.className='group-copy';var nm=document.createElement('div');nm.className='group-name';nm.textContent=g.name;
      var subtitle=document.createElement('div');subtitle.className='group-caption';subtitle.textContent=(locale&&locale.language()==='en'?(g.kind==='subscription'?'Subscription · ':'Configurations · '):(g.kind==='subscription'?'Подписка · ':'Конфигурации · '))+localeNumber(list.length);
      copy.appendChild(nm);copy.appendChild(subtitle);
      var chevron=document.createElement('span');chevron.className='group-chevron';chevron.innerHTML='<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>';
      toggle.appendChild(avatar);toggle.appendChild(copy);toggle.appendChild(chevron);
      toggle.onclick=function(){collapsed[g.id]=!collapsed[g.id];try{localStorage.setItem('cvpn_connect_collapsed',JSON.stringify(collapsed));}catch(e){}renderProfiles(allProfiles);};
      head.appendChild(toggle);
      if(g.canRefresh){var refreshBtn=document.createElement('button');refreshBtn.type='button';refreshBtn.className='group-refresh-button';refreshBtn.title='Обновить подписку';refreshBtn.setAttribute('aria-label','Обновить '+g.name);refreshBtn.setAttribute('data-group-id',g.id);refreshBtn.innerHTML='<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 11a8 8 0 1 0-2.35 5.66"/><path d="M20 4v7h-7"/></svg>';refreshBtn.onclick=function(){modalGroup=g;groupAction('refresh');};head.appendChild(refreshBtn);}
      if(g.kind==='subscription'){var pingBtn=document.createElement('button');pingBtn.type='button';pingBtn.className='group-refresh-button cvpn-ping-button';pingBtn.title=isEN()?'Measure TCP latency from TV':'Проверить TCP-задержку с телевизора';pingBtn.setAttribute('aria-label',isEN()?'Check server latency':'Проверить задержку серверов');pingBtn.innerHTML='<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 16a9 9 0 0 1 18 0"/><path d="m12 16 4-6"/><circle cx="12" cy="16" r="1"/></svg>';pingBtn.onclick=function(){probeSubscription(g,pingBtn);};head.appendChild(pingBtn);}
      var menu=document.createElement('button');menu.className='group-menu-button';menu.type='button';menu.setAttribute('aria-label','Управление группой '+g.name);menu.textContent='⋯';menu.onclick=function(){openGroupMenu(g);};head.appendChild(menu);
      wrapper.appendChild(head);
      if(isOfficialCVPN(g)){var published=renderCVPNMeta(g);if(published)wrapper.appendChild(published);}
      if(!collapsed[g.id]){
        var serverList=document.createElement('div');serverList.className='server-list';
        list.forEach(function(p){
          var row=document.createElement('div');row.className='server-row';row.setAttribute('data-id',p.id);
          var main=document.createElement('button');main.type='button';main.className='server-main';main.setAttribute('data-server-name',p.name);main.setAttribute('aria-label','Подключить '+p.name);main.disabled=busy;
          main.onclick=function(){connectTo(p.id);};
          var flag=document.createElement('span');flag.className='server-symbol';flag.textContent=countryFor(p.name);
          var txt=document.createElement('span');txt.className='server-info';var sn=document.createElement('span');sn.className='server-name';sn.textContent=shortServerName(p,g);
          var meta=document.createElement('span');meta.className='server-protocol';meta.textContent='VLESS · '+p.transport+' · '+p.security;
          txt.appendChild(sn);txt.appendChild(meta);
          var badge=document.createElement('span');badge.className='server-selection';badge.textContent='';
          main.appendChild(flag);main.appendChild(txt);main.appendChild(badge);
          var info=document.createElement('button');info.type='button';info.className='server-info-button';info.textContent='ⓘ';info.title='Сведения о сервере';info.setAttribute('aria-label','Сведения о '+p.name);info.onclick=function(){openServerInfo(p,g);};
          row.appendChild(main);
          var probe=subscriptionProbeResults[g.id]&&subscriptionProbeResults[g.id][p.id];
          if(probe){var lag=document.createElement('span');lag.className='cvpn-server-latency'+(probe.reachable?' is-online':' is-offline');lag.textContent=probe.reachable?(probe.ms+' ms'):'—';lag.title=probe.reachable?(isEN()?'TCP latency (not a VPN handshake)':'TCP-задержка (не проверка VPN-подключения)'):(isEN()?'TCP unavailable':'TCP недоступен');row.appendChild(lag);}
          row.appendChild(info);serverList.appendChild(row);
        });wrapper.appendChild(serverList);
      }
      target.appendChild(wrapper);
    });
    renderStatus(currentStatus);
  }
  function renderSubscriptions(){
    var root=$('subscription-list');root.textContent='';
    (allProfiles.groups||[]).filter(function(g){return g.kind!=='installed'&&g.id!=='installed';}).forEach(function(group){
      var count=(allProfiles.items||[]).filter(function(p){return p.groupId===group.id&&p.id!=='installed';}).length;
      if(!count)return;
      var item=document.createElement('button');item.type='button';item.className='subscription-item';
      var avatar=makeGroupAvatar(group,'subscription-avatar');
      var inner=document.createElement('span');inner.className='subscription-content';
      var title=document.createElement('strong');title.textContent=group.name;
      var detail=document.createElement('small');detail.textContent=(locale&&locale.language()==='en'?(group.kind==='subscription'?'Subscription · ':'Configurations · '):(group.kind==='subscription'?'Подписка · ':'Конфигурации · '))+localeNumber(count)+(humanUpdated(group)?' · '+humanUpdated(group):'');
      inner.appendChild(title);inner.appendChild(detail);
      if(isOfficialCVPN(group) && group.meta){var sm=group.meta,summary=[];
        if(sm.expiresAt){var d=providerDate(sm.expiresAt);if(d)summary.push((isEN()?'Until ':'До ')+d);}
        else if(sm.neverExpires)summary.push(isEN()?'No expiry':'Без срока');
        if(sm.updateHours)summary.push((isEN()?'Suggested refresh ':'Интервал ')+sm.updateHours+(isEN()?' h':' ч'));
        if(summary.length){var subnote=document.createElement('small');subnote.className='cvpn-provider-subscription-note';subnote.textContent=summary.join(' · ');inner.appendChild(subnote);}
      }
      var arrow=document.createElement('span');arrow.className='subscription-arrow';arrow.innerHTML='<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg>';
      item.appendChild(avatar);item.appendChild(inner);item.appendChild(arrow);
      item.onclick=function(){openGroupMenu(group);};root.appendChild(item);
    });
    if(!(allProfiles.items||[]).filter(function(x){return x.id!=='installed';}).length){var note=document.createElement('div');note.className='subscriptions-hint';note.textContent='Конфигураций пока нет. Добавьте подписку или VLESS-ссылку.';root.appendChild(note);}
  }
  function switchTab(tab){
    if(['home','subscriptions','settings'].indexOf(tab)<0)tab='home';currentTab=tab;
    ['home','subscriptions','settings'].forEach(function(name){
      visible('tab-'+name,name===tab);
      var btn=$('nav-'+name);btn.classList.toggle('active',name===tab);
      if(name===tab)btn.setAttribute('aria-current','page');else btn.removeAttribute('aria-current');
    });
  }
  function refresh(){if(!accessKey&&!csrf)return Promise.resolve();return Promise.all([api('profiles'),api('status')]).then(function(all){renderProfiles(all[0]);renderStatus(all[1]);loadGeoIPStatus();loadAutostartStatus();visible('login',false);visible('dashboard',true);$('settings-trigger').disabled=false;visible('settings-trigger',true);$('add-toggle').disabled=false;$('details-toggle').disabled=false;lastKnownOnline=true;return all;}).catch(function(e){if(accessKey){alertUser(e.message,true);}throw e;});}
  function poll(){if((!accessKey&&!csrf)||polling||busy||$('dashboard').hidden)return;polling=true;api('status').then(function(s){lastKnownOnline=true;renderStatus(s);}).catch(function(){lastKnownOnline=false;$('panel-health').className='online-mini';$('panel-health').textContent='Связь потеряна';$('connection-state').textContent='Статус неизвестен';$('connection-detail').textContent='Не удалось получить состояние VPN';$('disconnect').disabled=true;visible('trial-hint',false);}).then(function(){polling=false;});}
  function connectTo(id){if(busy||!lastKnownOnline)return;var p=(allProfiles.items||[]).filter(function(x){return x.id===id;})[0];if(!p)return;
    if(currentStatus&&currentStatus.connected&&allProfiles.active===id){alertUser('Этот сервер уже подключён');return;}
    errorUntil=0;pendingAction='connecting';pendingTarget=p.name;setBusy(true);renderStatus(currentStatus||{connected:false});
    api('connect','POST',{id:id}).then(function(result){
      updatePending('checking',p.name);
      // Keep the Engine's 90-second rollback guard. After it reports a
      // healthy connection, automatically confirm in the *same user action*.
      // A failure still exposes the manual confirm fallback.
      if(result.status&&result.status.connected&&result.status.mode==='trial'){
        return api('engine','POST',{action:'confirm'}).catch(function(e){
          alertUser('VPN работает, но подтверждение не завершилось: '+e.message,true);
          return null;
        });
      }
    }).then(function(){return refresh();}).then(function(){
      if(currentStatus&&currentStatus.mode==='trial')alertUser('Подключение активно — подтвердите его для продолжения.',true);
      else alertUser('VPN подключён.');
    }).catch(function(err){return refresh().catch(function(){}).then(function(){displayError(err.message);});}).then(function(){pendingAction='';pendingTarget='';setBusy(false);renderStatus(currentStatus);poll();});
  }
  function disconnect(){if(busy)return;pendingAction='disconnecting';pendingTarget='';setBusy(true);renderStatus(currentStatus);api('engine','POST',{action:'stop'}).then(function(){return refresh();}).then(function(){alertUser('VPN отключён. Сетевые параметры восстановлены.');}).catch(function(err){displayError(err.message);refresh().catch(function(){});}).then(function(){pendingAction='';setBusy(false);renderStatus(currentStatus);});}
  function confirmTrial(){if(busy)return;pendingAction='confirming';setBusy(true);renderStatus(currentStatus);api('engine','POST',{action:'confirm'}).then(function(){return refresh();}).then(function(){alertUser('Пробное подключение подтверждено.');}).catch(function(e){displayError(e.message);}).then(function(){pendingAction='';setBusy(false);renderStatus(currentStatus);});}
  var views=['import-view','group-view','rename-view','delete-view','info-view'];
  function openModal(title,view){modalReturnFocus=document.activeElement;$('modal-title').textContent=title;views.forEach(function(v){visible(v,v===view);});visible('modal-overlay',true);document.body.classList.add('no-scroll');$('modal-close').focus();}
  function changeModal(title,view){$('modal-title').textContent=title;views.forEach(function(v){visible(v,v===view);});}
  function closeModal(){visible('modal-overlay',false);document.body.classList.remove('no-scroll');if(modalReturnFocus&&modalReturnFocus.focus)modalReturnFocus.focus();}
  function openGroupMenu(g){modalGroup=g;openModal(g.name,'group-view');$('group-summary').textContent=(g.kind==='subscription'?'Подписка':g.kind==='installed'?'Исходный профиль':'Группа')+' · '+localeNumber(g.count||0)+(g.domain?' · '+g.domain:'');visible('group-refresh',g.canRefresh);$('group-refresh').disabled=!g.canRefresh;$('group-delete').disabled=g.kind==='installed';var isSub=g.kind==='subscription',isManual=g.id==='manual';$('group-delete').innerHTML=(isSub?'Удалить подписку':isManual?'Очистить группу':'Удалить группу')+' <span>⌫</span>';}
  function showImportReport(result,title){
    var rows=[['Получено',String(result.received||0)],['Принято',String(result.valid===undefined?(result.imported||0):result.valid)],['Добавлено',String(result.added===undefined?(result.imported||0):result.added)],['Пропущено',(result.issues||[]).length+''],['Сохранено старых',String(result.retained||0)],['Удалено',String(result.removed||0)]];
    (result.issues||[]).forEach(function(issue){rows.push([issue.server||'Сервер',issue.reason||'Не прошёл проверку']);});
    (result.removedNames||[]).forEach(function(n){rows.push(['Удалён из подписки',n+' — отсутствует в обновлённом списке']);});
    if(result.partial){rows.push(['Внимание','Часть ссылок не прошла проверку. Ранее сохранённые серверы оставлены без удаления.']);}
    openModal(title,'info-view');fillInfo(rows);
  }
  function groupAction(action, extra){if(busy||!modalGroup)return;setBusy(true);var id=modalGroup.id;var refreshButtons=document.querySelectorAll('.group-refresh-button');Array.prototype.forEach.call(refreshButtons,function(btn){btn.disabled=true;if(action==='refresh'&&btn.getAttribute('data-group-id')===id){btn.classList.add('is-refreshing');btn.setAttribute('aria-busy','true');}});return api('groups/'+action,'POST',Object.assign({id:id},extra||{})).then(function(result){return refresh().then(function(){return result;});}).then(function(result){closeModal();if(action==='refresh'){showImportReport(result,'Отчёт об обновлении');}else{var isSub=modalGroup&&modalGroup.kind==='subscription',isManual=modalGroup&&modalGroup.id==='manual';alertUser(action==='rename'?'Название сохранено':isSub?'Подписка удалена':isManual?'Группа очищена':'Группа удалена');}}).catch(function(e){if(e.issues&&e.issues.length){showImportReport({issues:e.issues},'Ошибки подписки');}alertUser(e.message,true);}).then(function(){setBusy(false);Array.prototype.forEach.call(document.querySelectorAll('.group-refresh-button'),function(btn){btn.disabled=false;btn.classList.remove('is-refreshing');btn.removeAttribute('aria-busy');});});}
  function openServerInfo(p,g){modalServer=p;openModal('Сведения о сервере','info-view');fillInfo([['Название',p.name],['Сервис',g.name],['Протокол','VLESS'],['Транспорт',p.transport],['Защита',p.security],['Адрес сервера',p.address||'—'],['Состояние',currentStatus&&currentStatus.connected&&allProfiles.active===p.id?'Подключён':allProfiles.active===p.id?'Выбран':'Не выбран']]);}
  function fillInfo(entries){var list=$('info-list');list.textContent='';entries.forEach(function(pair){var row=document.createElement('div'),dt=document.createElement('dt'),dd=document.createElement('dd');dt.textContent=pair[0];dd.textContent=pair[1];row.appendChild(dt);row.appendChild(dd);list.appendChild(row);});}
  function detectType(value){var v=value.trim().toLowerCase();if(v.indexOf('https://')===0)return 'HTTPS-подписка · название определится автоматически';if(v.indexOf('vless://')===0)return 'VLESS · один сервер';if(v.charAt(0)==='{')return 'JSON · конфигурация Xray';return 'Вставьте HTTPS, VLESS или JSON';}
  // UI-only preferences and local TV link. Never touch VPN Engine.
  function displayPanelAddress(){var url=location.protocol+'//'+location.host+'/';$('settings-panel-address').textContent=location.host||'Локальный адрес';return url;}
  function setReducedMotion(on){document.documentElement.classList.toggle('cvpn-reduce-motion',!!on);try{localStorage.setItem('cvpn_reduce_motion',on?'1':'0');}catch(e){}}
  try{$('settings-reduce-motion').checked=localStorage.getItem('cvpn_reduce_motion')==='1';}catch(e){}
  setReducedMotion($('settings-reduce-motion').checked);
  $('settings-reduce-motion').onchange=function(){setReducedMotion(this.checked);};
  displayPanelAddress();
  if(locale){
    $('settings-language').value=locale.get();
    $('settings-language').onchange=function(){
      locale.set(this.value);
      renderProfiles(allProfiles);
      if(currentStatus)renderStatus(currentStatus);
      if(accessKey||csrf){api('language','POST',{language:this.value}).catch(function(err){alertUser(err.message,true);});}
    };
  }

  $('settings-copy-address').onclick=function(){var url=displayPanelAddress();
    if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(url).then(function(){alertUser('Адрес панели скопирован');}).catch(function(){window.prompt(locale?locale.translate('Скопируйте адрес панели:'):'Скопируйте адрес панели:',url);});}
    else{window.prompt(locale?locale.translate('Скопируйте адрес панели:'):'Скопируйте адрес панели:',url);}
  };
  $('login-form').onsubmit=function(e){e.preventDefault();
    var code=$('pairing-code').value.replace(/\s/g,'');
    if(!/^[0-9]{6}$/.test(code)){$('login-error').textContent='Введите шестизначный код с телевизора';return;}
    stopPairPoll();$('login-error').textContent='';$('pair-button').disabled=true;
    $('pairing-progress').textContent='Проверяем код…';
    requestPair('pairing/request','POST',{code:code,name:detectedDeviceName()}).then(function(r){
       if(r.state==='approved'){stopPairPoll();$('pairing-progress').textContent='Телефон подключён';authenticate();return;}
       pairPending={id:r.id,token:r.token};pollPair();pairingTimer=setInterval(pollPair,2200);
    }).catch(function(err){stopPairPoll();$('login-error').textContent=err.message;$('pairing-progress').textContent='';});
  };
  $('logout').onclick=function(){api('auth/logout','POST',{}).catch(function(){}).then(function(){showLogin();});};
  function loadDevices(){api('devices').then(function(result){var root=$('devices-list');root.textContent='';(result.items||[]).forEach(function(d){
    var item=document.createElement('div');item.className='device-item';var name=document.createElement('div');name.textContent=d.name+(d.current?' · это устройство':'');
    var btn=document.createElement('button');btn.type='button';btn.className='secondary small';btn.textContent='Отозвать';btn.onclick=function(){
      if(!window.confirm(locale&&locale.language()==='en'?('Revoke access for “'+d.name+'”?'):('Отозвать доступ устройства «'+d.name+'»?')))return;
      api('devices/revoke','POST',{id:d.id}).then(function(){if(d.current){showLogin();}else{loadDevices();}alertUser('Доступ отозван');}).catch(function(e){alertUser(e.message,true);});
    };item.appendChild(name);item.appendChild(btn);root.appendChild(item);
  });if(!(result.items||[]).length){root.textContent='Подключённых устройств пока нет';}}).catch(function(e){alertUser(e.message,true);});}
  // Restore only CVPN-owned sessions. No direct route or DNS changes from JS.
  function recoveryCheck(){
    var btn=$('recovery-check'), fix=$('recovery-restore');
    btn.disabled=true;fix.disabled=true;
    $('recovery-readout').textContent='Проверяем состояние…';
    return api('recovery/check').then(function(s){
      var messages={
        clean:'Обычная сеть работает, следов активного VPN нет. Восстановление не требуется.',
        owned_session:'Найдена активная сессия CVPN. Можно безопасно отключить VPN и вернуть обычную сеть.',
        busy:'Выполняется другая операция CVPN. Дождитесь её завершения.',
        incomplete_session:'Данные активной VPN-сессии неполные. Автоматическое восстановление заблокировано.',
        orphaned_resources:'Обнаружены остаточные сетевые ресурсы без подтверждённой сессии. Нужна ручная диагностика по SSH.'
      };
      $('recovery-readout').textContent=messages[s.state]||'Не удалось определить состояние сети.';
      fix.disabled=!s.canRestore;
      return s;
    }).catch(function(e){$('recovery-readout').textContent=e.message;alertUser(e.message,true);}).then(function(v){btn.disabled=false;return v;});
  }
  $('help-toggle').onclick=function(){var show=$('help-content').hidden;visible('help-content',show);$('help-toggle').setAttribute('aria-expanded',show?'true':'false');if(show)recoveryCheck();};
  $('recovery-check').onclick=function(){recoveryCheck();};
  $('recovery-restore').onclick=function(){
    var english=locale&&locale.language()==='en';
    var prompt=english?'Disconnect CVPN and restore the TV’s regular network?':'Отключить VPN и восстановить обычную сеть телевизора?';
    if(!window.confirm(prompt))return;
    var btn=$('recovery-restore');btn.disabled=true;$('recovery-check').disabled=true;
    $('recovery-readout').textContent='Восстанавливаем обычную сеть…';
    api('recovery/restore','POST',{confirm:true}).then(function(result){
      $('recovery-readout').textContent=result.alreadyClean?'Сеть уже в обычном состоянии.':'Обычная сеть восстановлена. VPN отключён.';
      return refresh().then(function(){alertUser('Обычная сеть восстановлена');});
    }).catch(function(e){$('recovery-readout').textContent=e.message;alertUser(e.message,true);})
      .then(function(){recoveryCheck();});
  };
  $('devices-toggle').onclick=function(){var show=$('devices-panel').hidden;visible('devices-panel',show);$('devices-toggle').setAttribute('aria-expanded',show?'true':'false');if(show)loadDevices();};
  $('devices-refresh').onclick=loadDevices;
  $('settings-trigger').onclick=function(){switchTab('settings');$('details-content').hidden=false;$('details-toggle').setAttribute('aria-expanded','true');$('details-toggle').scrollIntoView({behavior:'smooth',block:'start'});loadLogs();};
  $('details-toggle').onclick=function(){var open=$('details-content').hidden;visible('details-content',open);$('details-toggle').setAttribute('aria-expanded',open?'true':'false');if(open)loadLogs();};
  $('refresh').onclick=function(){refresh().then(function(){alertUser('Данные обновлены');}).catch(function(){});};$('diagnostics-update').onclick=function(){refresh().then(function(){loadLogs();alertUser('Диагностика обновлена');}).catch(function(){});};
  function loadLogs(){api('logs').then(function(r){$('logs').textContent=r.logs||'Событий нет';}).catch(function(err){$('logs').textContent=err.message;});}
  $('logs-refresh').onclick=loadLogs;
  $('network-toggle').onclick=function(){var yes=$('network-info').hidden;visible('network-info',yes);$('network-toggle').setAttribute('aria-expanded',yes?'true':'false');if(yes){api('diagnostics').then(function(r){$('network-info').textContent='ДВИЖОК\n'+(r.engineVersion||'—')+'\nПРОВЕРКА ИНТЕРНЕТА\n'+(r.httpsDetail||'—')+'\nDNS\n'+(r.dns||'—')+'\n'+(r.dnsExplanation||'')+'\nМАРШРУТЫ\n'+(r.route||'—')+'\nИНТЕРФЕЙСЫ\n'+(r.interfaces||'—');}).catch(function(err){$('network-info').textContent=err.message;});}};
  $('disconnect').onclick=disconnect;$('confirm').onclick=confirmTrial;$('retry').onclick=function(){connectTo(allProfiles.active);};
  $('add-toggle').onclick=function(){openModal('Добавить конфигурацию','import-view');$('import-message').textContent='';$('import-content').focus();};
  $('subs-add').onclick=function(){$('add-toggle').click();};
  ['home','subscriptions','settings'].forEach(function(tab){$('nav-'+tab).onclick=function(){switchTab(tab);if(tab==='settings'){loadLogs();loadGeoIPStatus();loadAutostartStatus();}window.scrollTo({top:0,behavior:'smooth'});};});

  function renderAutostartStatus(d){
    if(!d)return;
    $('autostart-vpn-switch').checked=!!d.vpnAutoConnect;
    $('autostart-details').hidden=!d.vpnAutoConnect;
    $('autostart-badge').textContent=d.vpnAutoConnect?'Активно':'Отключено';
    $('autostart-badge').className=d.vpnAutoConnect?'settings-connected':'settings-connected muted';
    if(d.delaySeconds)$('autostart-delay').value=String(d.delaySeconds);
    $('autostart-target-server').textContent=d.activeProfile||'Не выбран';
    $('autostart-target-ip').textContent=d.activeProfileIp||'—';
    var webEl=$('autostart-web-address');
    if(webEl)webEl.textContent=d.webAddress||(location.origin||'—');
    $('autostart-notify-switch').checked=d.notifyOnStartup!==false;
    $('autostart-panel-switch').checked=!!d.panelHookEnabled;
    $('autostart-last-run').textContent=d.lastAutoConnect?(formatDate(d.lastAutoConnect)+' ('+d.lastStatus+')'):(d.lastStatus||'—');
  }
  function loadAutostartStatus(){
    if(!accessKey&&!csrf)return Promise.resolve();
    return api('autostart').then(function(d){renderAutostartStatus(d);return d;}).catch(function(e){console.warn('Autostart error:',e);});
  }
  function saveAutostartSettings(){
    var vpn=$('autostart-vpn-switch').checked;
    var delay=parseInt($('autostart-delay').value,10)||30;
    var hook=$('autostart-panel-switch').checked;
    var notify=$('autostart-notify-switch').checked;
    $('autostart-details').hidden=!vpn;
    return api('autostart','POST',{vpnAutoConnect:vpn,delaySeconds:delay,panelHookEnabled:hook,notifyOnStartup:notify}).then(function(res){
      renderAutostartStatus(res);
      var msg=$('autostart-status-msg');
      if(msg){msg.hidden=false;msg.className='geoip-status-msg success';msg.textContent='Настройки автозапуска сохранены';setTimeout(function(){msg.hidden=true;},4000);}
    }).catch(function(e){
      alertUser('Ошибка автозапуска: '+e.message,true);
    });
  }
  $('autostart-vpn-switch').onchange=saveAutostartSettings;
  $('autostart-delay').onchange=saveAutostartSettings;
  $('autostart-notify-switch').onchange=saveAutostartSettings;
  $('autostart-panel-switch').onchange=saveAutostartSettings;

  var testNotifyBtn=$('autostart-test-notify-btn');
  if(testNotifyBtn){
    testNotifyBtn.onclick=function(){
      var statusEl=$('autostart-test-notify-status');
      testNotifyBtn.disabled=true;
      if(statusEl){statusEl.textContent='Отправка...';statusEl.className='muted';}
      api('autostart/test-notify','POST',{}).then(function(){
        if(statusEl){statusEl.textContent='Уведомление показано на ТВ';statusEl.className='success';}
      }).catch(function(err){
        if(statusEl){statusEl.textContent='Ошибка: '+(err.message||err);statusEl.className='error';}
      }).finally(function(){
        setTimeout(function(){
          testNotifyBtn.disabled=false;
          if(statusEl)statusEl.textContent='';
        },4000);
      });
    };
  }

  var geoIPData=null;
  function formatBytes(bytes){if(!bytes||bytes<=0)return 'нет';return (bytes/(1024*1024)).toFixed(1)+' МБ';}
  function formatDate(isoStr){if(!isoStr)return '—';try{var d=new Date(isoStr);if(isNaN(d.getTime()))return isoStr;return d.toLocaleDateString()+' '+d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});}catch(e){return isoStr;}}
  function setGeoIPMessage(text,type){var el=$('geoip-status-msg');if(!el)return;if(!text){el.hidden=true;el.textContent='';el.className='geoip-status-msg';return;}el.hidden=false;el.className='geoip-status-msg '+(type||'info');el.textContent=text;}
  function renderGeoIPStatus(d){
    if(!d)return;geoIPData=d;
    $('geoip-status-badge').textContent=d.enabled?'Активно':'Отключено';
    $('geoip-status-badge').className=d.enabled?'settings-connected':'settings-connected error';
    $('geoip-file-info').textContent=(d.geoipSize>0)?(formatBytes(d.geoipSize)+' ('+formatDate(d.geoipModified)+')'):'Не найдена';
    $('geosite-file-info').textContent=(d.geositeSize>0)?(formatBytes(d.geositeSize)+' ('+formatDate(d.geositeModified)+')'):'Не найдена';
    var lastCheckText='—';
    if(d.lastSuccess){lastCheckText='Обновлено '+formatDate(d.lastSuccess);}
    else if(d.lastCheck){lastCheckText='Проверено '+formatDate(d.lastCheck);}
    if(d.lastStatus&&d.lastStatus!=='успешно обновлено'&&d.lastStatus!=='не проверялось'){lastCheckText+=' ('+d.lastStatus+')';}
    $('geoip-last-check').textContent=lastCheckText;
    $('geoip-autoupdate').checked=!!d.autoUpdate;
    if(d.updateIntervalDays)$('geoip-interval').value=String(d.updateIntervalDays);
    if(d.geoipUrl)$('geoip-url-input').value=d.geoipUrl;
    if(d.geositeUrl)$('geosite-url-input').value=d.geositeUrl;
    if(d.updating){
      $('geoip-update-btn').disabled=true;
      $('geoip-update-btn').textContent=d.updateText||'Обновление…';
    }else{
      $('geoip-update-btn').disabled=false;
      $('geoip-update-btn').textContent='Обновить базы сейчас';
    }
  }
  function loadGeoIPStatus(){
    if(!accessKey&&!csrf)return Promise.resolve();
    return api('geoip/status').then(function(d){renderGeoIPStatus(d);return d;}).catch(function(e){console.warn('GeoIP status error:',e);});
  }
  $('geoip-toggle-urls').onclick=function(){
    var panel=$('geoip-sources-panel');
    var isHidden=panel.hidden;
    panel.hidden=!isHidden;
    $('geoip-toggle-urls').textContent=isHidden?'Источники баз ▴':'Источники баз ▾';
  };
  $('geoip-update-btn').onclick=function(){
    var btn=$('geoip-update-btn');
    btn.disabled=true;
    btn.textContent='Загрузка баз…';
    setGeoIPMessage('Скачиваем свежие базы geoip.dat и geosite.dat…','info');
    api('geoip/update','POST').then(function(res){
      setGeoIPMessage(res.message||'Базы успешно обновлены!','success');
      return loadGeoIPStatus();
    }).catch(function(e){
      setGeoIPMessage('Ошибка обновления: '+e.message,'error');
      btn.disabled=false;
      btn.textContent='Обновить базы сейчас';
    });
  };
  $('geoip-save-settings').onclick=function(){
    var autoUpdate=$('geoip-autoupdate').checked;
    var interval=parseInt($('geoip-interval').value,10)||7;
    var geoipUrl=$('geoip-url-input').value.trim();
    var geositeUrl=$('geosite-url-input').value.trim();
    setGeoIPMessage('Сохраняем настройки…','info');
    api('geoip/settings','POST',{autoUpdate:autoUpdate,updateIntervalDays:interval,geoipUrl:geoipUrl,geositeUrl:geositeUrl}).then(function(res){
      renderGeoIPStatus(res);
      setGeoIPMessage('Настройки маршрутизации сохранены','success');
    }).catch(function(e){
      setGeoIPMessage('Ошибка: '+e.message,'error');
    });
  };
  $('geoip-reset-urls').onclick=function(){
    if(!geoIPData)return;
    $('geoip-url-input').value=geoIPData.defaultGeoipUrl||'';
    $('geosite-url-input').value=geoIPData.defaultGeositeUrl||'';
    $('geoip-autoupdate').checked=true;
    $('geoip-interval').value='7';
    $('geoip-save-settings').click();
  };
  $('modal-close').onclick=closeModal;
  $('modal-overlay').onclick=function(e){if(e.target===$('modal-overlay'))closeModal();};
  document.addEventListener('keydown',function(e){if(e.key==='Escape'&&!$('modal-overlay').hidden){e.preventDefault();closeModal();}});
  $('import-content').oninput=function(){$('detected-type').textContent=detectType(this.value);};
  $('paste-button').onclick=function(){if(navigator.clipboard&&navigator.clipboard.readText){navigator.clipboard.readText().then(function(text){$('import-content').value=text;$('import-content').oninput();}).catch(function(){alertUser('Вставьте ссылку через меню браузера или удержание поля.');$('import-content').focus();});}else{$('import-content').focus();alertUser('Для вставки используйте Ctrl+V или удерживайте поле.');}};
  $('import-form').onsubmit=function(e){e.preventDefault();var content=$('import-content').value.trim();if(!content)return;var button=$('import-submit');button.disabled=true;button.textContent='Проверяем конфигурации…';$('import-message').textContent='Загрузка и проверка Xray…';
    api('import','POST',{content:content}).then(function(out){if(!out.alreadyExists){$('import-content').value='';$('import-content').oninput();}return refresh().then(function(){return out;});}).then(function(result){if(result.alreadyExists){closeModal();alertUser('Подписка уже добавлена. Обновите её через меню группы.');}else{showImportReport(result,'Результат импорта');}}).catch(function(err){$('import-message').textContent=err.message;if(err.issues&&err.issues.length){showImportReport({issues:err.issues},'Ошибки импорта');}alertUser(err.message,true);}).then(function(){button.disabled=false;button.textContent='Добавить конфигурацию →';});};
  $('group-refresh').onclick=function(){groupAction('refresh');};
  $('group-rename').onclick=function(){changeModal('Переименовать группу','rename-view');$('rename-input').value=modalGroup.name;$('rename-input').focus();};
  $('rename-form').onsubmit=function(e){e.preventDefault();groupAction('rename',{name:$('rename-input').value});};
  $('group-delete').onclick=function(){
    var isSub=modalGroup&&modalGroup.kind==='subscription';
    var isManual=modalGroup&&modalGroup.id==='manual';
    var title=isSub?'Удалить подписку?':isManual?'Очистить группу?':'Удалить группу?';
    var desc=isSub?('Удалить подписку «'+modalGroup.name+'» и все входящие в неё серверы? Это действие нельзя отменить.'):isManual?'Удалить все сохранённые серверы из этой группы? Это действие нельзя отменить.':('Удалить группу «'+modalGroup.name+'» и все связанные серверы? Это действие нельзя отменить.');
    var el=$('delete-modal-desc');if(el)el.textContent=desc;
    changeModal(title,'delete-view');
  };
  $('cancel-delete').onclick=function(){changeModal(modalGroup.name,'group-view');};
  $('confirm-delete').onclick=function(){groupAction('delete');};
  $('group-info').onclick=function(){changeModal('Информация о группе','info-view');fillInfo([['Название',modalGroup.name],['Тип',modalGroup.kind==='subscription'?'HTTPS-подписка':modalGroup.kind==='installed'?'Исходный профиль':'Локальная группа'],['Серверов',String(modalGroup.count||0)],['Источник',modalGroup.domain||'Локально'],['Последнее обновление',modalGroup.updatedAt||'Неизвестно']]);};
  if (qrPin) { $('pairing-code').value=qrPin;syncPIN(); $('pairing-progress').textContent='Код из QR добавлен. Нажмите «Подключить устройство».'; }
  authenticate();
  setInterval(poll,3500);
  window.addEventListener('focus',poll);document.addEventListener('visibilitychange',function(){if(!document.hidden)poll();});
})();
